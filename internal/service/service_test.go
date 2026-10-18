package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cduhub/internal/model"
)

type fakeCollector struct {
	discover func(context.Context) ([]model.Source, []string)
	crawl    func(context.Context, model.Source, model.SyncOptions) (model.Source, []model.Article, error)
}

func (f fakeCollector) Discover(ctx context.Context) ([]model.Source, []string) {
	if f.discover != nil {
		return f.discover(ctx)
	}
	return nil, nil
}

func (f fakeCollector) CrawlSource(ctx context.Context, s model.Source, opts model.SyncOptions) (model.Source, []model.Article, error) {
	if f.crawl != nil {
		return f.crawl(ctx, s, opts)
	}
	s.Status = "ok"
	return s, nil, nil
}

func newTestService(t *testing.T, path string, collector Collector) *Service {
	t.Helper()
	s, err := NewWithCollector(path, collector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func awaitSync(t *testing.T, s *Service) model.Snapshot {
	t.Helper()
	s.mu.RLock()
	done := s.done
	s.mu.RUnlock()
	select {
	case <-done:
		return s.Snapshot()
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not finish")
		return model.Snapshot{}
	}
}

func TestIncrementalSyncPreservesDatesAndPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "snapshot.json")
	source := model.Source{ID: "office", Name: "学校办公室", URL: "https://xzbgs.cdu.edu.cn/", Kind: "department", Status: "pending"}
	channel := model.Channel{ID: "notice", Name: "通知通告", URL: source.URL + "tztg.htm"}
	var calls atomic.Int32
	collector := fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) { return []model.Source{source}, nil },
		crawl: func(_ context.Context, s model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			s.Status = "ok"
			if calls.Add(1) == 1 {
				s.Channels = []model.Channel{channel}
				return s, []model.Article{
					{ID: "a", SourceID: s.ID, Title: "原始标题", Date: "2026-09-13", DateText: "2026年9月13日", DateInferred: true},
					{ID: "old", SourceID: s.ID, Title: "历史通知", Date: "2025-01-02", DateText: "2025-01-02"},
				}, nil
			}
			s.Channels = nil
			return s, []model.Article{
				{ID: "a", SourceID: s.ID, Title: "更新后的标题", DateText: "09-13"},
				{ID: "new", SourceID: s.ID, Title: "新增通知", Date: "2026-09-14", DateText: "2026-09-14"},
				{ID: "new", SourceID: s.ID, Title: "新增通知", Date: "2026-09-14", DateText: "2026-09-14"},
			}, nil
		},
	}
	s := newTestService(t, path, collector)
	for round := 0; round < 2; round++ {
		if err := s.Start(model.SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		if snapshot := awaitSync(t, s); snapshot.Sync.Phase != "completed" {
			t.Fatalf("sync %d: %+v", round, snapshot.Sync)
		}
	}
	snapshot := s.Snapshot()
	if len(snapshot.Articles) != 3 || snapshot.Sources[0].ArticleCount != 3 {
		t.Fatalf("incremental merge lost or duplicated articles: %+v", snapshot)
	}
	if got := []string{snapshot.Articles[0].ID, snapshot.Articles[1].ID, snapshot.Articles[2].ID}; !reflect.DeepEqual(got, []string{"new", "a", "old"}) {
		t.Fatalf("articles should be ordered by publication date, got %v", got)
	}
	updated := snapshot.Articles[1]
	if updated.Title != "更新后的标题" || updated.Date != "2026-09-13" || updated.DateText != "2026年9月13日" || !updated.DateInferred {
		t.Fatalf("refresh should update title without degrading a known full date: %+v", updated)
	}
	if !reflect.DeepEqual(snapshot.Sources[0].Channels, []model.Channel{channel}) {
		t.Fatalf("transient missing column list erased existing columns: %+v", snapshot.Sources[0])
	}
	restarted := newTestService(t, path, collector).Snapshot()
	if !reflect.DeepEqual(snapshot, restarted) {
		t.Fatalf("restart changed persisted results:\nbefore=%+v\nafter=%+v", snapshot, restarted)
	}
}

func TestFailuresPreservePartialResultsAndOtherSources(t *testing.T) {
	sources := []model.Source{
		{ID: "good", Name: "可访问学院", Status: "pending"},
		{ID: "bad", Name: "暂时不可访问部门", Status: "pending", Channels: []model.Channel{{ID: "prior", Name: "通知公告"}}},
	}
	s := newTestService(t, filepath.Join(t.TempDir(), "snapshot.json"), fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return sources, []string{"一个目录暂时不可访问"}
		},
		crawl: func(_ context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			article := model.Article{ID: source.ID + "-article", SourceID: source.ID, Title: "已经获取的通知"}
			if source.ID == "bad" {
				return model.Source{}, []model.Article{article}, errors.New("upstream timeout")
			}
			source.Status = "ok"
			return source, []model.Article{article}, nil
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Phase != "partial" || snapshot.Sync.Running || snapshot.Sync.Completed != 2 || snapshot.Sync.Total != 2 {
		t.Fatalf("failure should be isolated to its source: %+v", snapshot.Sync)
	}
	if len(snapshot.Articles) != 2 || len(snapshot.Sync.Errors) != 2 {
		t.Fatalf("partial articles and discovery warnings must survive: %+v", snapshot)
	}
	for _, source := range snapshot.Sources {
		if source.ArticleCount != 1 {
			t.Errorf("wrong article count for %s: %d", source.ID, source.ArticleCount)
		}
		if source.ID == "bad" && (source.Status != "error" || source.Error != "upstream timeout" || len(source.Channels) != 1) {
			t.Errorf("failure lost the source identity or saved columns: %+v", source)
		}
		if source.ID == "good" && source.Status != "ok" {
			t.Errorf("healthy source marked as failed: %+v", source)
		}
	}
}

func TestConcurrentStartAllowsOneRunAndCancelAllowsRestart(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	var discoveries atomic.Int32
	s := newTestService(t, filepath.Join(t.TempDir(), "snapshot.json"), fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			discoveries.Add(1)
			return []model.Source{{ID: "office", Name: "办公室"}}, nil
		},
		crawl: func(ctx context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return source, []model.Article{{ID: "cancelled-result", SourceID: source.ID}}, ctx.Err()
			}
			source.Status = "ok"
			return source, nil, nil
		},
	})
	const contenders = 16
	var wg sync.WaitGroup
	gate := make(chan struct{})
	results := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-gate; results <- s.Start(model.SyncOptions{}) }()
	}
	close(gate)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected one accepted concurrent start, got %d", successes)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("collector never started")
	}
	s.Cancel()
	s.Cancel()
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Phase != "cancelled" || snapshot.Sync.Running || len(snapshot.Articles) != 0 {
		t.Fatalf("cancelled in-flight result should not be committed: %+v", snapshot)
	}
	if err := s.Start(model.SyncOptions{SourceID: "office"}); err != nil {
		t.Fatalf("restart after cancellation: %v", err)
	}
	if restarted := awaitSync(t, s); restarted.Sync.Phase != "completed" || restarted.Sync.Completed != 1 {
		t.Fatalf("single-source restart failed: %+v", restarted.Sync)
	}
	if discoveries.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("single-source refresh should reuse its known source: discoveries=%d crawls=%d", discoveries.Load(), calls.Load())
	}
}

func TestStartRejectsInvalidOptionsBeforeCallingCollector(t *testing.T) {
	var calls atomic.Int32
	s := newTestService(t, "", fakeCollector{discover: func(context.Context) ([]model.Source, []string) { calls.Add(1); return nil, nil }})
	for _, options := range []model.SyncOptions{
		{PagesPerChannel: -1}, {PagesPerChannel: 501},
		{MaxChannelsPerSource: -1}, {MaxChannelsPerSource: 201},
		{SourceID: "unknown"},
	} {
		if err := s.Start(options); err == nil {
			t.Errorf("accepted invalid options: %+v", options)
		}
	}
	if calls.Load() != 0 || s.Snapshot().Sync.Running {
		t.Fatal("invalid request started a crawl")
	}
}

func TestCancellationKeepsAlreadySavedResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	blocked := make(chan struct{})
	s := newTestService(t, path, fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return []model.Source{{ID: "fast", Name: "已完成学院"}, {ID: "slow", Name: "正在抓取学院"}}, nil
		},
		crawl: func(ctx context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			if source.ID == "slow" {
				close(blocked)
				<-ctx.Done()
				return source, nil, ctx.Err()
			}
			source.Status = "ok"
			return source, []model.Article{{ID: "saved", SourceID: source.ID, Title: "已获取的通知"}}, nil
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("second source did not start")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for s.Snapshot().Sync.Completed != 1 {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("completed source was not saved during sync")
		}
	}
	s.Cancel()
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Phase != "cancelled" || snapshot.Sync.Completed != 1 || len(snapshot.Articles) != 1 || snapshot.Articles[0].ID != "saved" {
		t.Fatalf("cancellation erased completed work: %+v", snapshot)
	}
	restarted := newTestService(t, path, fakeCollector{}).Snapshot()
	if !reflect.DeepEqual(snapshot, restarted) {
		t.Fatal("cancelled partial results did not survive restart")
	}
}

func TestSnapshotIsAnIndependentCopy(t *testing.T) {
	s := newTestService(t, "", fakeCollector{})
	s.state.Sources = []model.Source{{ID: "source", Channels: []model.Channel{{Name: "通知公告"}}}}
	s.state.Articles = []model.Article{{Title: "公开通知"}}
	s.state.Sync.Errors = []string{"original error"}
	copy := s.Snapshot()
	copy.Sources[0].Channels[0].Name = "changed"
	copy.Sources[0].Name = "changed"
	copy.Articles[0].Title = "changed"
	copy.Sync.Errors[0] = "changed"
	current := s.Snapshot()
	if current.Sources[0].Channels[0].Name != "通知公告" || current.Sources[0].Name != "" || current.Articles[0].Title != "公开通知" || current.Sync.Errors[0] != "original error" {
		t.Fatal("caller mutated the live state through Snapshot")
	}
}

func TestRestartMarksInterruptedRunAndPreservesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	data, err := json.Marshal(model.Snapshot{Sync: model.SyncStatus{Running: true, Phase: "crawling"}, Articles: []model.Article{{ID: "saved"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := newTestService(t, path, fakeCollector{}).Snapshot()
	if snapshot.Sync.Running || snapshot.Sync.Phase != "interrupted" || len(snapshot.Articles) != 1 || snapshot.Sources == nil || snapshot.Sync.Errors == nil {
		t.Fatalf("incorrect interrupted-run recovery: %+v", snapshot)
	}
	const corrupt = "{truncated cache"
	if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWithCollector(path, fakeCollector{}); err == nil || !strings.Contains(err.Error(), "保留原文件") {
		t.Fatalf("expected explicit corrupt-cache error, got %v", err)
	}
	remaining, err := os.ReadFile(path)
	if err != nil || string(remaining) != corrupt {
		t.Fatalf("corrupt cache was overwritten: %q, %v", remaining, err)
	}
}
