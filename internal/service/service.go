package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cduhub/internal/crawler"
	"cduhub/internal/model"
)

type Collector interface {
	Discover(context.Context) ([]model.Source, []string)
	CrawlSource(context.Context, model.Source, model.SyncOptions) (model.Source, []model.Article, error)
}

type Service struct {
	mu        sync.RWMutex
	state     model.Snapshot
	path      string
	collector Collector
	cancel    context.CancelFunc
	done      chan struct{}
}

func New(path string) (*Service, error) { return NewWithCollector(path, crawler.New()) }

func NewWithCollector(path string, collector Collector) (*Service, error) {
	s := &Service{path: path, collector: collector, state: model.Snapshot{Sources: []model.Source{}, Articles: []model.Article{}, Sync: model.SyncStatus{Phase: "idle", Errors: []string{}}}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, fmt.Errorf("读取本地数据失败（保留原文件）: %w", err)
		}
		s.state.Sync.Running = false
		if s.state.Sync.Phase == "discovering" || s.state.Sync.Phase == "crawling" {
			s.state.Sync.Phase = "interrupted"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.state.Sources == nil {
		s.state.Sources = []model.Source{}
	}
	if s.state.Articles == nil {
		s.state.Articles = []model.Article{}
	}
	if s.state.Sync.Errors == nil {
		s.state.Sync.Errors = []string{}
	}
	return s, nil
}

func (s *Service) Snapshot() model.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.state
	out.Sources = make([]model.Source, len(s.state.Sources))
	copy(out.Sources, s.state.Sources)
	for i := range out.Sources {
		out.Sources[i].Channels = append([]model.Channel{}, out.Sources[i].Channels...)
	}
	out.Articles = make([]model.Article, len(s.state.Articles))
	copy(out.Articles, s.state.Articles)
	out.Sync.Errors = append([]string{}, s.state.Sync.Errors...)
	return out
}

func (s *Service) Start(options model.SyncOptions) error {
	if options.PagesPerChannel == 0 {
		options.PagesPerChannel = 2
	}
	if options.MaxChannelsPerSource == 0 {
		options.MaxChannelsPerSource = 40
	}
	if options.PagesPerChannel < 1 || options.PagesPerChannel > 50 {
		return errors.New("每栏目页数需在 1 到 50 之间")
	}
	if options.MaxChannelsPerSource < 1 || options.MaxChannelsPerSource > 100 {
		return errors.New("每站栏目数需在 1 到 100 之间")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Sync.Running {
		return errors.New("已有同步任务正在运行")
	}
	if options.SourceID != "" {
		found := false
		for _, source := range s.state.Sources {
			if source.ID == options.SourceID {
				found = true
				break
			}
		}
		if !found {
			return errors.New("未找到指定来源")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.state.Sync = model.SyncStatus{Running: true, Phase: "discovering", StartedAt: now(), Errors: []string{}}
	go s.run(ctx, options, s.done)
	return nil
}

func (s *Service) Cancel() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) Close() {
	s.mu.RLock()
	cancel, done := s.cancel, s.done
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Service) run(ctx context.Context, options model.SyncOptions, done chan struct{}) {
	defer close(done)
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state.Sync.Running = false
		s.state.Sync.FinishedAt = now()
		s.state.Sync.Current = ""
		s.state.Sync.Phase = "completed"
		if ctx.Err() != nil {
			s.state.Sync.Phase = "cancelled"
		} else if len(s.state.Sync.Errors) > 0 {
			s.state.Sync.Phase = "partial"
		}
		if err := s.saveLocked(); err != nil {
			s.state.Sync.Errors = append(s.state.Sync.Errors, "保存失败: "+err.Error())
			s.state.Sync.Phase = "error"
		}
	}()
	if options.SourceID == "" {
		sources, warnings := s.collector.Discover(ctx)
		s.mu.Lock()
		s.state.Sync.Errors = append(s.state.Sync.Errors, warnings...)
		existing := map[string]int{}
		for i, source := range s.state.Sources {
			existing[source.ID] = i
		}
		for _, source := range sources {
			if i, ok := existing[source.ID]; ok {
				s.state.Sources[i].Name = source.Name
				s.state.Sources[i].URL = source.URL
				s.state.Sources[i].Kind = source.Kind
				s.state.Sources[i].Group = source.Group
			} else {
				existing[source.ID] = len(s.state.Sources)
				s.state.Sources = append(s.state.Sources, source)
			}
		}
		s.mu.Unlock()
	}
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	var pending []model.Source
	for _, source := range s.state.Sources {
		if options.SourceID == "" || source.ID == options.SourceID {
			pending = append(pending, source)
		}
	}
	s.state.Sync.Total = len(pending)
	s.state.Sync.Phase = "crawling"
	if len(pending) == 0 {
		s.state.Sync.Errors = append(s.state.Sync.Errors, "未发现可抓取来源，请检查网络后重试")
	}
	s.mu.Unlock()
	jobs := make(chan model.Source)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for source := range jobs {
				if ctx.Err() != nil {
					return
				}
				s.mu.Lock()
				s.state.Sync.Current = source.Name
				s.mu.Unlock()
				updated, articles, err := s.collector.CrawlSource(ctx, source, options)
				if ctx.Err() != nil {
					return
				}
				s.mu.Lock()
				if err != nil {
					s.state.Sync.Errors = append(s.state.Sync.Errors, source.Name+": "+err.Error())
					if updated.Status == "" || updated.Status == "ok" {
						updated.Status = "error"
					}
					updated.Error = err.Error()
				}
				if updated.ID == "" {
					updated = source
					updated.Status = "error"
					if err != nil {
						updated.Error = err.Error()
					}
				}
				if len(updated.Channels) == 0 && len(source.Channels) > 0 {
					updated.Channels = source.Channels
				}
				index := make(map[string]int, len(s.state.Articles))
				for i, a := range s.state.Articles {
					index[a.ID] = i
				}
				for _, a := range articles {
					if i, ok := index[a.ID]; ok {
						old := s.state.Articles[i]
						if a.Date == "" && old.Date != "" {
							a.Date = old.Date
							a.DateText = old.DateText
							a.DateInferred = old.DateInferred
						}
						if a.Content == "" && old.Content != "" {
							a.Content = old.Content
						}
						s.state.Articles[i] = a
					} else {
						index[a.ID] = len(s.state.Articles)
						s.state.Articles = append(s.state.Articles, a)
					}
				}
				updated.ArticleCount = 0
				for _, a := range s.state.Articles {
					if a.SourceID == updated.ID {
						updated.ArticleCount++
					}
				}
				for i, v := range s.state.Sources {
					if v.ID == updated.ID {
						s.state.Sources[i] = updated
						break
					}
				}
				sort.SliceStable(s.state.Articles, func(i, j int) bool {
					a, b := s.state.Articles[i], s.state.Articles[j]
					if a.Date == b.Date {
						return a.ID < b.ID
					}
					return a.Date > b.Date
				})
				s.state.Sync.Completed++
				s.state.LastUpdated = now()
				if err := s.saveLocked(); err != nil {
					s.state.Sync.Errors = append(s.state.Sync.Errors, "保存失败: "+err.Error())
				}
				s.mu.Unlock()
			}
		}()
	}
feed:
	for _, source := range pending {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- source:
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *Service) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func now() string { return time.Now().Format(time.RFC3339) }
