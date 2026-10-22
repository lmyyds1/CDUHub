package service

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"cduhub/internal/model"
)

// seedLargeState 构造一份接近真实规模的快照（约 1.7 万篇文章、73 个来源），
// 用于观测「每个来源完成时的写锁内成本」与「快照读成本」。
//
// 真实数据参考 research/review/backend.md：16753 篇文章 / 73 个来源 / 磁盘 9.67 MB。
func seedLargeState(tb testing.TB, path string, articleCount, sourceCount int) (*Service, model.Source) {
	tb.Helper()
	s, err := NewWithCollector(path, fakeCollector{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(s.Close)

	s.mu.Lock()
	defer s.mu.Unlock()
	perSource := articleCount / sourceCount
	for i := 0; i < sourceCount; i++ {
		sourceID := fmt.Sprintf("src-%02d", i)
		s.state.Sources = append(s.state.Sources, model.Source{ID: sourceID, Name: fmt.Sprintf("来源 %d", i), Status: "ok"})
		for j := 0; j < perSource; j++ {
			s.state.Articles = append(s.state.Articles, model.Article{
				ID:         fmt.Sprintf("%s-%04d", sourceID, j),
				SourceID:   sourceID,
				Title:      fmt.Sprintf("通知 %d-%d", i, j),
				Content:    "公开摘要内容，用于模拟真实快照体积。",
				Date:       fmt.Sprintf("2026-%02d-%02d", j%12+1, j%28+1),
				FetchedAt:  "2026-09-14T10:00:00+08:00",
				SourceName: fmt.Sprintf("来源 %d", i),
			})
		}
	}
	s.rebuildIndexesLocked()
	target := s.state.Sources[len(s.state.Sources)-1]
	target.Channels = []model.Channel{{ID: "notice", Name: "通知通告", URL: "https://example.invalid/tztg.htm"}}
	return s, target
}

// benchmarkBatch 模拟一个来源一次返回的文章：一半是新增，一半刷新既有条目。
func benchmarkBatch(target model.Source, existing int) []model.Article {
	batch := make([]model.Article, 0, 230)
	for i := 0; i < existing; i++ {
		batch = append(batch, model.Article{ID: fmt.Sprintf("%s-%04d", target.ID, i), SourceID: target.ID, Title: "刷新后的通知", Date: "2026-09-14"})
	}
	for i := 0; i < 230-existing; i++ {
		batch = append(batch, model.Article{ID: fmt.Sprintf("%s-new-%03d", target.ID, i), SourceID: target.ID, Title: "新通知", Date: "2026-09-13"})
	}
	return batch
}

// BenchmarkMergeSourceLargeState 衡量「一个来源完成时的写锁内工作量」。
// 修复前该区间还要重建 1.7 万条索引 + 全量重算计数 + 全量排序 + 序列化 9.67 MB。
func BenchmarkMergeSourceLargeState(b *testing.B) {
	s, target := seedLargeState(b, "", 16753, 73)
	batch := benchmarkBatch(target, 50)
	updated := target
	updated.Status = "ok"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.mu.Lock()
		s.mergeSourceLocked(target, updated, batch, nil)
		s.mu.Unlock()
	}
}

// BenchmarkLegacyLockedStepsReference 是修复前锁内三步的参考实现
// （全量建索引 + 全量重算 ArticleCount + 全量 SliceStable 排序），
// 仅用于在同一台机器上给出「前后对比」的下界，不参与生产逻辑。
// 它不包含修复前同样在锁内执行的 10MB MarshalIndent+写盘，那部分见 BenchmarkPersistLargeState。
func BenchmarkLegacyLockedStepsReference(b *testing.B) {
	s, target := seedLargeState(b, "", 16753, 73)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.mu.Lock()
		index := make(map[string]int, len(s.state.Articles))
		for j, article := range s.state.Articles {
			index[article.ID] = j
		}
		count := 0
		for _, article := range s.state.Articles {
			if article.SourceID == target.ID {
				count++
			}
		}
		sort.SliceStable(s.state.Articles, func(i, j int) bool {
			a, c := s.state.Articles[i], s.state.Articles[j]
			if a.Date == c.Date {
				return a.ID < c.ID
			}
			return a.Date > c.Date
		})
		s.mu.Unlock()
		_ = index
		_ = count
	}
}

// BenchmarkSnapshotLargeState 衡量读侧成本（深拷贝 + 锁外排序）。
func BenchmarkSnapshotLargeState(b *testing.B) {
	s, _ := seedLargeState(b, "", 16753, 73)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snapshot := s.Snapshot(); len(snapshot.Articles) == 0 {
			b.Fatal("empty snapshot")
		}
	}
}

// BenchmarkPersistLargeState 衡量一次落盘的真实成本（MarshalIndent + 原子替换），
// 用来解释为什么必须把保存从「每个来源一次」改成「去抖合并」。
func BenchmarkPersistLargeState(b *testing.B) {
	path := filepath.Join(b.TempDir(), "snapshot.json")
	s, _ := seedLargeState(b, path, 16753, 73)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.mu.Lock()
		s.stateSeq++ // 模拟状态变更，强制真正写盘
		s.mu.Unlock()
		if err := s.persist(); err != nil {
			b.Fatal(err)
		}
	}
}
