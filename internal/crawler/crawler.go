// Package crawler discovers public Chengdu University sites and collects article metadata.
package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cduhub/internal/model"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	DepartmentDirectory = "https://www.cdu.edu.cn/zzjg.htm"
	CollegeDirectory    = "https://www.cdu.edu.cn/yxsz.htm"
	maxBodyBytes        = 4 << 20
	defaultPages        = 2
	defaultChannels     = 40
	detailBudget        = 12
)

type Crawler struct {
	client      *http.Client
	mu          sync.Mutex
	nextRequest map[string]time.Time
	interval    time.Duration
	semaphore   chan struct{}
}

func New() *Crawler {
	c := &Crawler{nextRequest: make(map[string]time.Time), interval: 250 * time.Millisecond, semaphore: make(chan struct{}, 8)}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 3
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 3
	transport.ResponseHeaderTimeout = 12 * time.Second
	c.client = &http.Client{Timeout: 18 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return errors.New("网站重定向次数过多")
		}
		if !officialURL(req.URL.String()) {
			return errors.New("重定向离开成都大学官方域名")
		}
		if protectedURL(req.URL.String()) {
			return errors.New("该页面需要统一身份认证")
		}
		return nil
	}}
	return c
}

func (c *Crawler) wait(ctx context.Context, raw string) error {
	u, _ := url.Parse(raw)
	c.mu.Lock()
	now := time.Now()
	when := c.nextRequest[u.Hostname()]
	if when.Before(now) {
		when = now
	}
	c.nextRequest[u.Hostname()] = when.Add(c.interval)
	c.mu.Unlock()
	timer := time.NewTimer(time.Until(when))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Crawler) fetch(ctx context.Context, raw string) (*html.Node, string, error) {
	if !officialURL(raw) {
		return nil, raw, errors.New("仅允许成都大学官方 HTTP(S) 网址")
	}
	if protectedURL(raw) {
		return nil, raw, errors.New("该页面需要登录，不采集认证内容")
	}
	if err := c.wait(ctx, raw); err != nil {
		return nil, raw, err
	}
	select {
	case c.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, raw, ctx.Err()
	}
	defer func() { <-c.semaphore }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, raw, err
	}
	req.Header.Set("User-Agent", "CDUInfoHub/1.0 (+public university information aggregator; metadata only)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	response, err := c.client.Do(req)
	if err != nil {
		return nil, raw, err
	}
	defer response.Body.Close()
	final := response.Request.URL.String()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, final, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	contentType := response.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "html") && !strings.Contains(strings.ToLower(contentType), "text/plain") {
		return nil, final, fmt.Errorf("非 HTML 页面：%s", contentType)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return nil, final, err
	}
	if len(body) > maxBodyBytes {
		return nil, final, errors.New("页面超过 4 MiB 限制")
	}
	decoded, err := charset.NewReader(strings.NewReader(string(body)), contentType)
	if err != nil {
		return nil, final, fmt.Errorf("解码页面：%w", err)
	}
	doc, err := html.Parse(decoded)
	if err != nil {
		return nil, final, err
	}
	return doc, final, nil
}

// Discover reads the two university directories. Warnings preserve per-directory failures.
func (c *Crawler) Discover(ctx context.Context) ([]model.Source, []string) {
	type result struct {
		sources []model.Source
		warning string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, entry := range []struct{ url, kind string }{{DepartmentDirectory, "department"}, {CollegeDirectory, "college"}} {
		wg.Add(1)
		go func(i int, raw, kind string) {
			defer wg.Done()
			doc, final, err := c.fetch(ctx, raw)
			if err != nil {
				results[i].warning = fmt.Sprintf("目录 %s：%v", raw, err)
				return
			}
			results[i].sources = parseDirectory(doc, final, kind)
			if len(results[i].sources) == 0 {
				results[i].warning = "未能识别官方目录结构：" + raw
			}
		}(i, entry.url, entry.kind)
	}
	wg.Wait()
	sources := []model.Source{}
	warnings := []string{}
	for _, result := range results {
		sources = append(sources, result.sources...)
		if result.warning != "" {
			warnings = append(warnings, result.warning)
		}
	}
	return sources, warnings
}

func (c *Crawler) CrawlSource(ctx context.Context, source model.Source, opts model.SyncOptions) (model.Source, []model.Article, error) {
	source.LastCrawled = time.Now().UTC().Format(time.RFC3339)
	source.Error = ""
	if source.URL == "" {
		source.Status = "unavailable"
		source.Error = "官网目录未提供可访问的网址"
		return source, []model.Article{}, errors.New(source.Error)
	}
	pages := opts.PagesPerChannel
	if pages <= 0 {
		pages = defaultPages
	}
	if pages > 50 {
		pages = 50
	}
	limit := opts.MaxChannelsPerSource
	if limit <= 0 {
		limit = defaultChannels
	}
	if limit > 100 {
		limit = 100
	}
	doc, base, err := c.fetch(ctx, source.URL)
	if err != nil {
		source.Status = "error"
		source.Error = err.Error()
		return source, []model.Article{}, err
	}
	articles := []model.Article{}
	byURL := map[string]int{}
	merge := func(items []model.Article) {
		for _, item := range items {
			if index, exists := byURL[item.URL]; exists {
				old := &articles[index]
				if old.Date == "" && item.Date != "" {
					old.Date, old.DateText = item.Date, item.DateText
				}
				if old.DateText == "" {
					old.DateText = item.DateText
				}
				if len(item.Content) > len(old.Content) {
					old.Content = item.Content
				}
				if (strings.Contains(old.Title, "...") || strings.Contains(old.Title, "…")) && !strings.Contains(item.Title, "...") && !strings.Contains(item.Title, "…") {
					old.Title = item.Title
				}
				if item.Channel != "首页动态" {
					old.Channel = item.Channel
				}
			} else {
				byURL[item.URL] = len(articles)
				articles = append(articles, item)
			}
		}
	}
	merge(parseArticles(doc, base, source, "首页动态", source.LastCrawled))
	channels := []model.Channel{}
	knownChannels := map[string]bool{}
	addChannels := func(items []model.Channel) {
		for _, channel := range items {
			if knownChannels[channel.URL] || len(channels) >= limit {
				continue
			}
			knownChannels[channel.URL] = true
			channels = append(channels, channel)
		}
	}
	addChannels(parseChannels(doc, base, source.ID))
	visited := map[string]bool{base: true, source.URL: true}
	warnings := []string{}
	for i := 0; i < len(channels); i++ {
		if ctx.Err() != nil {
			warnings = append(warnings, ctx.Err().Error())
			break
		}
		channel := channels[i]
		raw := channel.URL
		for page := 0; page < pages && raw != ""; page++ {
			if visited[raw] {
				break
			}
			visited[raw] = true
			pageDoc, final, fetchErr := c.fetch(ctx, raw)
			if fetchErr != nil {
				warnings = append(warnings, fmt.Sprintf("%s（%s）：%v", channel.Name, raw, fetchErr))
				break
			}
			visited[final] = true
			merge(parseArticles(pageDoc, final, source, channel.Name, source.LastCrawled))
			if page == 0 {
				addChannels(parseChannels(pageDoc, final, source.ID))
			}
			raw = nextPage(pageDoc, final)
		}
	}
	// A small shared budget enriches full dates/titles without fetching every article body.
	indices := make([]int, 0)
	for i, article := range articles {
		if article.Date == "" || strings.Contains(article.Title, "...") || strings.Contains(article.Title, "…") {
			indices = append(indices, i)
		}
	}
	sort.SliceStable(indices, func(i, j int) bool {
		truncated := func(s string) bool { return strings.Contains(s, "...") || strings.Contains(s, "…") }
		return truncated(articles[indices[i]].Title) && !truncated(articles[indices[j]].Title)
	})
	if len(indices) > detailBudget {
		indices = indices[:detailBudget]
	}
	var wg sync.WaitGroup
	jobs := make(chan int)
	var detailMu sync.Mutex
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				a := &articles[index]
				detail, _, fetchErr := c.fetch(ctx, a.URL)
				if fetchErr != nil {
					detailMu.Lock()
					warnings = append(warnings, fmt.Sprintf("详情补全（%s）：%v", a.URL, fetchErr))
					detailMu.Unlock()
					continue
				}
				title, date, original := detailMetadata(detail)
				if title != "" {
					a.Title = title
				}
				if date != "" {
					a.Date, a.DateText = date, original
				}
				if content := detailContent(detail); len(content) > len(a.Content) {
					a.Content = content
				}
			}
		}()
	}
	for _, index := range indices {
		if ctx.Err() != nil {
			break
		}
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	source.Channels = channels
	source.ArticleCount = len(articles)
	source.Status = "success"
	if len(articles) == 0 {
		source.Status = "empty"
		warnings = append(warnings, "页面可访问，但未识别出公开文章；可能使用动态加载或需要特定站点适配")
	}
	if len(warnings) > 0 {
		if len(articles) > 0 {
			source.Status = "partial"
		}
		if len(warnings) > 8 {
			warnings = append(warnings[:8], fmt.Sprintf("另有 %d 项访问错误", len(warnings)-8))
		}
		source.Error = strings.Join(warnings, "；")
		return source, articles, errors.New(source.Error)
	}
	return source, articles, nil
}
