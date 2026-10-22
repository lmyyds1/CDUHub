package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
					{ID: "a", SourceID: s.ID, Title: "原始标题", Date: "2026-09-13", DateText: "2026年9月13日"},
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
	if updated.Title != "更新后的标题" || updated.Date != "2026-09-13" || updated.DateText != "2026年9月13日" {
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
	for _, tc := range []struct {
		options model.SyncOptions
		want    error
	}{
		{model.SyncOptions{PagesPerChannel: -1}, ErrInvalidOptions},
		{model.SyncOptions{PagesPerChannel: 501}, ErrInvalidOptions},
		{model.SyncOptions{MaxChannelsPerSource: -1}, ErrInvalidOptions},
		{model.SyncOptions{MaxChannelsPerSource: 201}, ErrInvalidOptions},
		{model.SyncOptions{SourceID: "unknown"}, ErrSourceNotFound},
	} {
		err := s.Start(tc.options)
		if err == nil {
			t.Errorf("accepted invalid options: %+v", tc.options)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("options %+v: error %v is not %v", tc.options, err, tc.want)
		}
	}
	if calls.Load() != 0 || s.Snapshot().Sync.Running {
		t.Fatal("invalid request started a crawl")
	}
}

func TestStartAfterCloseIsRejected(t *testing.T) {
	s := newTestService(t, "", fakeCollector{})
	s.Close()
	if err := s.Start(model.SyncOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close should report ErrClosed, got %v", err)
	}
	s.Close() // 幂等，不能 panic 或阻塞
}

func TestCollectorPanicIsRecoveredAndMarksRunFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	s := newTestService(t, path, fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return []model.Source{{ID: "boom", Name: "会崩溃的来源"}, {ID: "fine", Name: "正常来源"}}, nil
		},
		crawl: func(_ context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			if source.ID == "boom" {
				panic("collector exploded")
			}
			source.Status = "ok"
			return source, []model.Article{{ID: "fine-article", SourceID: source.ID, Title: "正常通知"}}, nil
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Running || snapshot.Sync.Phase != "error" {
		t.Fatalf("a panicking source must end the round as error, not completed: %+v", snapshot.Sync)
	}
	joined := strings.Join(snapshot.Sync.Errors, "\n")
	if !strings.Contains(joined, "采集程序异常") || !strings.Contains(joined, "collector exploded") {
		t.Fatalf("panic was not recorded in sync errors: %q", joined)
	}
	if len(snapshot.Articles) != 1 || snapshot.Articles[0].ID != "fine-article" {
		t.Fatalf("healthy source results were lost after a panic: %+v", snapshot.Articles)
	}
	// 磁盘上也不能谎称这一轮正常完成，否则重启后中断/失败不可见。
	restarted := newTestService(t, path, fakeCollector{}).Snapshot()
	if restarted.Sync.Running || restarted.Sync.Phase != "error" {
		t.Fatalf("persisted state hides the panic: %+v", restarted.Sync)
	}
	for _, source := range restarted.Sources {
		if source.ID == "boom" && source.Status != "error" {
			t.Errorf("panicking source should be marked error: %+v", source)
		}
	}
}

func TestDiscoverPanicIsRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	s := newTestService(t, path, fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) { panic("directory exploded") },
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Running || snapshot.Sync.Phase != "error" || !strings.Contains(strings.Join(snapshot.Sync.Errors, "\n"), "directory exploded") {
		t.Fatalf("discovery panic not recovered as error phase: %+v", snapshot.Sync)
	}
	if restarted := newTestService(t, path, fakeCollector{}).Snapshot(); restarted.Sync.Phase != "error" {
		t.Fatalf("persisted phase should stay error, got %q", restarted.Sync.Phase)
	}
}

func TestCloseHasBoundedWaitWhenCollectorIgnoresContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := newTestService(t, filepath.Join(t.TempDir(), "snapshot.json"), fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return []model.Source{{ID: "stuck", Name: "不响应取消的来源"}}, nil
		},
		crawl: func(context.Context, model.Source, model.SyncOptions) (model.Source, []model.Article, error) {
			close(started)
			<-release // 刻意忽略 ctx
			return model.Source{}, nil, errors.New("released")
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("collector never started")
	}
	begin := time.Now()
	s.closeWithTimeout(200 * time.Millisecond)
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("Close waited %v without bound", elapsed)
	}
	close(release)
	awaitSync(t, s) // 让后台 run 收尾，避免测试结束时有 goroutine 在跑
}

func TestPersistFailuresSurfaceAsErrorPhase(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newTestService(t, filepath.Join(blocker, "snapshot.json"), fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return []model.Source{{ID: "office", Name: "办公室"}}, nil
		},
		crawl: func(_ context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			source.Status = "ok"
			return source, []model.Article{{ID: "a", SourceID: source.ID, Title: "通知"}}, nil
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Phase != "error" || !strings.Contains(strings.Join(snapshot.Sync.Errors, "\n"), "保存失败") {
		t.Fatalf("unwritable path should surface as error phase: %+v", snapshot.Sync)
	}
	if len(snapshot.Articles) != 1 {
		t.Fatalf("in-memory results should survive a failed save: %+v", snapshot.Articles)
	}
}

func TestSaveIsThrottledAcrossSourceCompletions(t *testing.T) {
	sources := make([]model.Source, 0, 8)
	for i := 0; i < 8; i++ {
		sources = append(sources, model.Source{ID: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("来源 %d", i), Status: "pending"})
	}
	s := newTestService(t, filepath.Join(t.TempDir(), "snapshot.json"), fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) { return sources, nil },
		crawl: func(_ context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			source.Status = "ok"
			return source, []model.Article{{ID: source.ID + "-a", SourceID: source.ID, Title: "通知"}}, nil
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitSync(t, s)
	if snapshot.Sync.Phase != "completed" || snapshot.Sync.Completed != len(sources) {
		t.Fatalf("sync did not complete: %+v", snapshot.Sync)
	}

	s.mu.RLock()
	writes := s.saveWrites
	s.mu.RUnlock()
	if writes == 0 {
		t.Fatal("final state was never written to disk")
	}
	if writes >= uint64(len(sources)) {
		t.Fatalf("save throttle ineffective: %d writes for %d source completions", writes, len(sources))
	}
}

func TestIncrementalIndexSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	round := atomic.Int32{}
	collector := fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return []model.Source{{ID: "office", Name: "办公室", Status: "pending"}}, nil
		},
		crawl: func(_ context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			source.Status = "ok"
			if round.Add(1) == 1 {
				return source, []model.Article{
					{ID: "a", SourceID: source.ID, Title: "第一条", Date: "2026-09-01"},
					{ID: "b", SourceID: source.ID, Title: "第二条", Date: "2026-09-02"},
				}, nil
			}
			return source, []model.Article{
				{ID: "a", SourceID: source.ID, Title: "第一条（更新）", Date: "2026-09-01"},
				{ID: "c", SourceID: source.ID, Title: "第三条", Date: "2026-09-03"},
			}, nil
		},
	}
	s := newTestService(t, path, collector)
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitSync(t, s)

	// 重启后索引与计数必须由磁盘状态重建，而不是继续沿用旧内存。
	restarted := newTestService(t, path, collector)
	if err := restarted.Start(model.SyncOptions{SourceID: "office"}); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitSync(t, restarted)
	if len(snapshot.Articles) != 3 {
		t.Fatalf("duplicate articles after restart: %+v", snapshot.Articles)
	}
	if snapshot.Sources[0].ArticleCount != 3 {
		t.Fatalf("article count not maintained per source: %+v", snapshot.Sources[0])
	}
	if got := []string{snapshot.Articles[0].ID, snapshot.Articles[1].ID, snapshot.Articles[2].ID}; !reflect.DeepEqual(got, []string{"c", "b", "a"}) {
		t.Fatalf("snapshot should stay ordered by date, got %v", got)
	}
	if snapshot.Articles[2].Title != "第一条（更新）" {
		t.Fatalf("refresh did not update the existing article: %+v", snapshot.Articles[2])
	}
}

func TestConcurrentSnapshotSurvivesSync(t *testing.T) {
	s := newTestService(t, filepath.Join(t.TempDir(), "snapshot.json"), fakeCollector{
		discover: func(context.Context) ([]model.Source, []string) {
			return []model.Source{{ID: "a", Name: "甲"}, {ID: "b", Name: "乙"}}, nil
		},
		crawl: func(_ context.Context, source model.Source, _ model.SyncOptions) (model.Source, []model.Article, error) {
			source.Status = "ok"
			return source, []model.Article{{ID: source.ID + "-article", SourceID: source.ID, Title: "通知", Date: "2026-09-14"}}, nil
		},
	})
	if err := s.Start(model.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if snapshot := s.Snapshot(); len(snapshot.Articles) > 2 {
					t.Error("snapshot returned more articles than exist")
					return
				}
			}
		}
	}()
	awaitSync(t, s)
	close(stop)
	readers.Wait()
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
