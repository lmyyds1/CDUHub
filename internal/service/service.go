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

// 哨兵错误：让 HTTP 层与 CLI 能用 errors.Is 区分「任务已在运行」「参数非法」
// 「来源不存在」「服务已关闭」，而不是把所有业务失败一律当成 409。
var (
	ErrInvalidOptions = errors.New("同步参数无效")
	ErrSyncRunning    = errors.New("已有同步任务正在运行")
	ErrSourceNotFound = errors.New("未找到指定来源")
	ErrClosed         = errors.New("服务已关闭")
)

const (
	syncWorkers = 4

	// saveDebounce 是两次「后台落盘」之间的最小间隔。同步过程中每个来源完成只标脏，
	// 由后台保存协程合并写盘：同一时间最多每 500ms 一次序列化（服务器实测量级：
	// 1.7 万条快照 MarshalIndent+原子写盘约 45–60ms），既保留 README 承诺的
	// 「每站完成后保存」，又避免 N 个 worker 同时完成时重复序列化整库。
	// 取消、结束、Close 都会强制同步落盘一次，崩溃最多丢失最近 500ms 完成的来源。
	saveDebounce = 500 * time.Millisecond

	// maxSyncErrors 限制随快照落盘的错误条数，避免长期运行后快照无限膨胀。
	maxSyncErrors = 200

	// defaultCloseTimeout 是 Close 等待后台同步收尾的上限，避免 Wails 退出时挂起。
	defaultCloseTimeout = 5 * time.Second
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
	closed    bool

	// 常驻增量索引，避免每处理一个来源就重建全量 map；均只在持有 mu 写锁时维护。
	articleIndex map[string]int // 文章 ID -> state.Articles 下标
	sourceCounts map[string]int // 来源 ID -> 该来源文章数（用于 ArticleCount）

	// 保存版本号：stateSeq 每次状态变更递增，savedSeq 记录最后一次成功落盘的版本。
	// 后台保存协程只在 stateSeq != savedSeq 时写盘，因此不会写出比磁盘更旧的状态。
	stateSeq uint64
	savedSeq uint64

	saveMu      sync.Mutex // 串行化「取快照 + 序列化 + 原子写盘」，保证写入顺序单调
	lastSaveErr string
	saveWrites  uint64 // 成功写盘次数（测试与排障观测用）

	saveReq  chan struct{} // 有变更时通知后台保存协程（容量 1，重复通知合并）
	saveStop chan struct{}
	saveDone chan struct{}

	// workerPanic 记录本轮回合同中发生的 panic，收尾时把 Phase 置为 error。
	workerPanic error
}

func New(path string) (*Service, error) { return NewWithCollector(path, crawler.New()) }

func NewWithCollector(path string, collector Collector) (*Service, error) {
	s := &Service{
		path:      path,
		collector: collector,
		state:     model.Snapshot{Sources: []model.Source{}, Articles: []model.Article{}, Sync: model.SyncStatus{Phase: "idle", Errors: []string{}}},
		saveReq:   make(chan struct{}, 1),
		saveStop:  make(chan struct{}),
		saveDone:  make(chan struct{}),
	}
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
	s.rebuildIndexesLocked()
	// 清理上次进程被强杀时可能残留的临时文件（不影响数据，只是 10MB 级垃圾）。
	if path != "" {
		_ = os.Remove(path + ".tmp")
	}
	go s.saveLoop()
	return s, nil
}

// rebuildIndexesLocked 在加载快照后重建增量索引与按来源计数。
// 计数以实际文章为准，可顺便修正旧版本快照里可能过期的 ArticleCount。
func (s *Service) rebuildIndexesLocked() {
	s.articleIndex = make(map[string]int, len(s.state.Articles))
	s.sourceCounts = make(map[string]int, len(s.state.Sources))
	for i, article := range s.state.Articles {
		s.articleIndex[article.ID] = i
		s.sourceCounts[article.SourceID]++
	}
	for i := range s.state.Sources {
		s.state.Sources[i].ArticleCount = s.sourceCounts[s.state.Sources[i].ID]
	}
}

func (s *Service) Snapshot() model.Snapshot {
	s.mu.RLock()
	out := s.copyStateLocked()
	s.mu.RUnlock()
	// 排序放在锁外：文章按发布日期倒序是展示/导出的契约，但不必在写锁内维护顺序。
	sortArticles(out.Articles)
	return out
}

// copyStateLocked 复制一份可脱离锁使用的快照（调用方必须持有 mu，读锁即可）。
func (s *Service) copyStateLocked() model.Snapshot {
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

func sortArticles(articles []model.Article) {
	// (date desc, id asc) 是全序比较，因此不需要稳定排序；sort.Slice 在 1.7 万条规模上
	// 比 SliceStable 快一个数量级，而这段排序已经挪到写锁之外，只影响读侧 CPU。
	sort.Slice(articles, func(i, j int) bool {
		a, b := articles[i], articles[j]
		if a.Date == b.Date {
			return a.ID < b.ID
		}
		return a.Date > b.Date
	})
}

func (s *Service) Start(options model.SyncOptions) error {
	if options.PagesPerChannel == 0 {
		options.PagesPerChannel = 2
	}
	if options.MaxChannelsPerSource == 0 {
		options.MaxChannelsPerSource = 40
	}
	if options.PagesPerChannel < 1 || options.PagesPerChannel > 50 {
		return fmt.Errorf("%w：每栏目页数需在 1 到 50 之间", ErrInvalidOptions)
	}
	if options.MaxChannelsPerSource < 1 || options.MaxChannelsPerSource > 100 {
		return fmt.Errorf("%w：每站栏目数需在 1 到 100 之间", ErrInvalidOptions)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.state.Sync.Running {
		return ErrSyncRunning
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
			return fmt.Errorf("%w：%s（可用来源 ID 可通过 GET /api/snapshot 查询）", ErrSourceNotFound, options.SourceID)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.workerPanic = nil
	s.state.Sync = model.SyncStatus{Running: true, Phase: "discovering", StartedAt: now(), Errors: []string{}}
	s.markDirtyLocked()
	go s.run(ctx, options, s.done)
	return nil
}

// Cancel 请求取消正在进行的同步；可重复调用（context.CancelFunc 幂等）。
func (s *Service) Cancel() {
	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

// Close 取消并等待后台同步收尾，最多等待 defaultCloseTimeout，避免 Wails 退出被挂住。
func (s *Service) Close() { s.closeWithTimeout(defaultCloseTimeout) }

func (s *Service) closeWithTimeout(timeout time.Duration) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			s.mu.Lock()
			s.appendErrorLocked("关闭等待超时，已放弃等待后台同步收尾")
			s.mu.Unlock()
		}
	}
	close(s.saveStop)
	<-s.saveDone
}

func (s *Service) run(ctx context.Context, options model.SyncOptions, done chan struct{}) {
	defer close(done)
	defer s.finishRun(ctx)

	if options.SourceID == "" {
		sources, warnings := s.collector.Discover(ctx)
		s.discoverLocked(sources, warnings)
	}
	if ctx.Err() != nil {
		return
	}

	pending := s.preparePendingLocked(options)

	jobs := make(chan model.Source)
	var wg sync.WaitGroup
	for i := 0; i < syncWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for source := range jobs {
				if ctx.Err() != nil {
					continue // 取消后只排空队列，不再启动新的采集
				}
				s.processSource(ctx, options, source)
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

// discoverLocked 把目录发现结果合并进本地来源列表：只更新 Name/URL/Kind/Group，
// 保留 Channels/Status/LastCrawled/ArticleCount，避免一次目录故障清空本地信息。
func (s *Service) discoverLocked(sources []model.Source, warnings []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, warning := range warnings {
		s.appendErrorLocked(warning)
	}
	existing := make(map[string]int, len(s.state.Sources))
	for i, source := range s.state.Sources {
		existing[source.ID] = i
	}
	for _, source := range sources {
		if i, ok := existing[source.ID]; ok {
			s.state.Sources[i].Name = source.Name
			s.state.Sources[i].URL = source.URL
			s.state.Sources[i].Kind = source.Kind
			s.state.Sources[i].Group = source.Group
			continue
		}
		existing[source.ID] = len(s.state.Sources)
		source.ArticleCount = s.sourceCounts[source.ID]
		s.state.Sources = append(s.state.Sources, source)
	}
	s.markDirtyLocked()
}

// preparePendingLocked 选出本轮要抓取的来源，并写入 crawling 进度起点。
func (s *Service) preparePendingLocked(options model.SyncOptions) []model.Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := make([]model.Source, 0, len(s.state.Sources))
	for _, source := range s.state.Sources {
		if options.SourceID == "" || source.ID == options.SourceID {
			pending = append(pending, source)
		}
	}
	s.state.Sync.Total = len(pending)
	s.state.Sync.Phase = "crawling"
	if len(pending) == 0 {
		s.appendErrorLocked("未发现可抓取来源，请检查网络后重试")
	}
	s.markDirtyLocked()
	return pending
}

// finishRun 统一收尾：正常结束、取消、panic 都从这里落定终态并强制同步保存一次。
// 它作为 run 的 defer 注册，因此 recover() 能捕获 run 主协程（例如 Discover）里的 panic。
func (s *Service) finishRun(ctx context.Context) {
	panicValue := recover()

	s.mu.Lock()
	s.state.Sync.Running = false
	s.state.Sync.FinishedAt = now()
	s.state.Sync.Current = ""
	switch {
	case panicValue != nil:
		s.appendErrorLocked(fmt.Sprintf("采集程序异常: %v", panicValue))
		s.state.Sync.Phase = "error"
	case s.workerPanic != nil:
		// worker 内的 panic 已被就地恢复，这里统一把本轮标记为失败，绝不谎报 completed。
		s.state.Sync.Phase = "error"
	case ctx.Err() != nil:
		s.state.Sync.Phase = "cancelled"
	case len(s.state.Sync.Errors) > 0:
		s.state.Sync.Phase = "partial"
	default:
		s.state.Sync.Phase = "completed"
	}
	s.markDirtyLocked()
	s.mu.Unlock()

	// 收尾必须同步落盘一次，保证「重启后状态一致 / 中断可识别」的既有语义。
	if err := s.persist(); err != nil {
		s.mu.Lock()
		s.recordSaveFailureLocked(err)
		s.state.Sync.Phase = "error"
		s.mu.Unlock()
	}
}

// processSource 处理单个来源。任何 panic（含 collector 内部）都就地恢复，
// 记录进 Sync.Errors 并继续处理其他来源，避免一个坏页面终止整个进程。
func (s *Service) processSource(ctx context.Context, options model.SyncOptions, source model.Source) {
	defer func() {
		if r := recover(); r != nil {
			s.recordWorkerPanic(fmt.Sprintf("采集 %s 时发生异常: %v", source.Name, r))
		}
	}()
	s.mu.Lock()
	s.state.Sync.Current = source.Name
	s.mu.Unlock()

	updated, articles, err, panicked := s.crawlSource(ctx, source, options)
	if ctx.Err() != nil {
		return // 取消后丢弃在途结果，不提交
	}
	s.mergeSource(source, updated, articles, err, panicked)
}

func (s *Service) crawlSource(ctx context.Context, source model.Source, options model.SyncOptions) (updated model.Source, articles []model.Article, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			updated = source
			updated.Status = "error"
			updated.Error = fmt.Sprintf("采集程序异常: %v", r)
			articles = nil
			err = errors.New(updated.Error)
			panicked = true
		}
	}()
	updated, articles, err = s.collector.CrawlSource(ctx, source, options)
	return
}

func (s *Service) recordWorkerPanic(message string) {
	s.mu.Lock()
	s.appendErrorLocked(message)
	if s.workerPanic == nil {
		s.workerPanic = errors.New(message)
	}
	s.markDirtyLocked()
	s.mu.Unlock()
}

// mergeSource 取写锁后合并单个来源的结果；panic 时记录到本轮状态，使收尾阶段把 Phase 置为 error。
func (s *Service) mergeSource(source, updated model.Source, articles []model.Article, err error, panicked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeSourceLocked(source, updated, articles, err)
	if panicked && s.workerPanic == nil {
		s.workerPanic = err
	}
}

// mergeSourceLocked 在写锁内合并单个来源的结果。锁内只做 O(本来源文章数) 的
// 索引查找/追加与 O(来源数) 的来源定位，不再重建索引、全量重算计数或全量排序。
func (s *Service) mergeSourceLocked(source, updated model.Source, articles []model.Article, err error) {
	if err != nil {
		s.appendErrorLocked(source.Name + ": " + err.Error())
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
	for _, article := range articles {
		s.mergeArticleLocked(article)
	}
	updated.ArticleCount = s.sourceCounts[updated.ID]
	for i, candidate := range s.state.Sources {
		if candidate.ID == updated.ID {
			s.state.Sources[i] = updated
			break
		}
	}
	s.state.Sync.Completed++
	s.state.LastUpdated = now()
	s.markDirtyLocked()
}

// mergeArticleLocked 用增量索引合并单篇文章；既有日期/摘要不被降级（保持原有语义）。
func (s *Service) mergeArticleLocked(article model.Article) {
	if i, ok := s.articleIndex[article.ID]; ok {
		old := s.state.Articles[i]
		if article.Date == "" && old.Date != "" {
			article.Date = old.Date
			article.DateText = old.DateText
		}
		if article.Content == "" && old.Content != "" {
			article.Content = old.Content
		}
		if old.SourceID != article.SourceID {
			if s.sourceCounts[old.SourceID] > 0 {
				s.sourceCounts[old.SourceID]--
			}
			s.sourceCounts[article.SourceID]++
		}
		s.state.Articles[i] = article
		return
	}
	s.articleIndex[article.ID] = len(s.state.Articles)
	s.sourceCounts[article.SourceID]++
	s.state.Articles = append(s.state.Articles, article)
}

// markDirtyLocked 标记状态有变更并唤醒后台保存协程（非阻塞，重复信号自动合并）。
// 调用方必须持有 mu 写锁。
func (s *Service) markDirtyLocked() {
	s.stateSeq++
	select {
	case s.saveReq <- struct{}{}:
	default:
	}
}

// appendErrorLocked 追加同步错误并做上限截断，避免长期运行后快照无限膨胀。
// 调用方必须持有 mu 写锁。
func (s *Service) appendErrorLocked(message string) {
	s.state.Sync.Errors = append(s.state.Sync.Errors, message)
	if len(s.state.Sync.Errors) > maxSyncErrors {
		s.state.Sync.Errors = s.state.Sync.Errors[len(s.state.Sync.Errors)-maxSyncErrors:]
	}
}

func (s *Service) recordSaveFailureLocked(err error) {
	message := "保存失败: " + err.Error()
	if message == s.lastSaveErr {
		return
	}
	s.lastSaveErr = message
	s.appendErrorLocked(message)
}

// saveLoop 是唯一的落盘写者：收到标脏信号后去抖 500ms 再写一次最新状态，
// 因此同一时间最多每 500ms 一次序列化 + 原子替换，且不会并发 Rename。
func (s *Service) saveLoop() {
	defer close(s.saveDone)
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}
	defer stopTimer()
	for {
		select {
		case <-s.saveStop:
			stopTimer()
			s.persistQuiet()
			return
		case <-s.saveReq:
			if timerC == nil {
				timer = time.NewTimer(saveDebounce)
				timerC = timer.C
			}
		case <-timerC:
			stopTimer()
			s.persistQuiet()
		}
	}
}

func (s *Service) persistQuiet() {
	if err := s.persist(); err != nil {
		s.mu.Lock()
		s.recordSaveFailureLocked(err)
		s.mu.Unlock()
	}
}

// persist 取当前状态快照并在锁外序列化+原子写盘。写盘由 saveMu 串行化，
// 且快照在拿到 saveMu 之后才取，因此后一次写盘必然对应不早于前一次的状态，
// 不会出现「旧状态覆盖新状态」。
func (s *Service) persist() error {
	if s.path == "" {
		s.mu.Lock()
		s.savedSeq = s.stateSeq
		s.mu.Unlock()
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	if s.stateSeq == s.savedSeq {
		s.mu.RUnlock()
		return nil
	}
	state := s.copyStateLocked()
	seq := s.stateSeq
	s.mu.RUnlock()

	sortArticles(state.Articles)
	data, err := json.MarshalIndent(state, "", "  ")
	if err == nil {
		err = writeFileAtomic(s.path, data)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		return err
	}
	s.savedSeq = seq
	s.saveWrites++
	s.lastSaveErr = ""
	return nil
}

// writeFileAtomic 保持既有「.tmp + Rename」原子保存语义，并补一次 fsync，
// 降低断电时丢失最后一次保存或留下半截文件的概率。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func now() string { return time.Now().Format(time.RFC3339) }
