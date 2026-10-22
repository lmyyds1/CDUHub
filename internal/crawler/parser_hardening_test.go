package crawler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"cduhub/internal/model"
)

// A column header often shares its row with a 「最新日期」 badge. parseChannels
// used to drop the column whenever the row aggregated a date, so the 通知公告
// entry disappeared and its articles were never crawled.
func TestChannelWithRowDateIsKept(t *testing.T) {
	body := `<nav>
<li><a href="tzgg.htm">通知公告</a><span>09-14</span></li>
<li><a href="xwdt.htm">新闻动态</a><span>2026-09-14</span></li>
</nav>
<ul><li><a href="info/1/2.htm">学校领导动态的长篇新闻标题</a><span>09-14</span></li></ul>`
	channels := parseChannels(parseFixture(t, body), "https://x.cdu.edu.cn/", "s1")
	names := map[string]string{}
	for _, channel := range channels {
		names[channel.URL] = channel.Name
	}
	if names["https://x.cdu.edu.cn/tzgg.htm"] != "通知公告" {
		t.Errorf("column with a row date was dropped: %+v", channels)
	}
	if names["https://x.cdu.edu.cn/xwdt.htm"] != "新闻动态" {
		t.Errorf("column with a full row date was dropped: %+v", channels)
	}
	for _, channel := range channels {
		if strings.Contains(channel.URL, "/info/") {
			t.Errorf("article link became a column: %+v", channel)
		}
	}
}

// Breadcrumbs, section entry pages and pagination labels are not articles. They
// were collected whenever the surrounding row happened to aggregate a date:
// 15+ snapshot entries such as zzb.cdu.edu.cn/index.htm (title 首页) prove it.
func TestNavigationChromeIsNotAnArticle(t *testing.T) {
	body := `<div class="position"><a href="index.htm">首页</a><span>2026-07-14</span></div>
<div class="nav-b">当前位置：<a href="index.htm">首页</a> &gt; <a href="bszn.htm">办事指南</a></div>
<ul>
<li><a href="xzzq1.htm">下载专区</a><span>2026-07-14</span></li>
<li><a href="bszn.htm">办事指南</a><span>2026-07-15</span></li>
<li><a href="lxwm.htm">联系我们</a><span>2026-07-16</span></li>
<li><a href="tzgg/2.htm">下一页</a></li>
<li><a href="index.htm">返回首页</a></li>
<li><a href="info/1052/1463.htm">成都大学期刊中心借力会议开展组稿约稿</a><span>2026-09-14</span></li>
<li><a href="1457.htm">下一条：成都大学期刊中心三本期刊获奖</a><span>2026-09-14</span></li>
<li><a href="1463.htm">《成都大学学报（自然科学版）》青年编委招募启事</a><span>2026-09-14</span></li>
</ul>
<div class="copyright">版权所有 © 2026-07-14 成都大学</div>`
	items := parseArticles(parseFixture(t, body), "https://journal.cdu.edu.cn/", model.Source{ID: "journal"}, "新闻动态", "")
	got := map[string]bool{}
	for _, item := range items {
		got[item.URL] = true
	}
	for _, bad := range []string{"index.htm", "xzzq1.htm", "bszn.htm", "lxwm.htm", "tzgg/2.htm", "1457.htm"} {
		if got["https://journal.cdu.edu.cn/"+bad] {
			t.Errorf("navigation page collected as an article: %s (%+v)", bad, items)
		}
	}
	// Documents with a plain URL name are still articles.
	for _, good := range []string{"info/1052/1463.htm", "1463.htm"} {
		if !got["https://journal.cdu.edu.cn/"+good] {
			t.Errorf("real document dropped: %s (%+v)", good, items)
		}
	}
}

// A detail page header <h1> usually repeats the site name. It must not shadow the
// real headline rendered as h2-h4 inside .art-tit/.cont-tit/.news-title.
func TestDetailTitlePrefersArticleHeadingOverSiteName(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "site-name h1 before the article heading",
			body: `<html><head><title>关于校园网维护的通知-成都大学信息网络中心</title></head><body>
<h1>成都大学信息网络中心</h1>
<div class="art-tit cont-tit"><h3>关于校园网维护的通知</h3><p><span>时间：2026-09-14 浏览量：32</span></p></div>
<div id="vsb_content" class="art-body"><div class="v_news_content"><p>正文</p></div></div></body></html>`,
			want: "关于校园网维护的通知",
		},
		{
			name: "news-title heading without any site-name suffix",
			body: `<body><h1>成都大学信息网络中心</h1><div class="news-title"><h2>校园网出口带宽升级公告</h2></div></body>`,
			want: "校园网出口带宽升级公告",
		},
		{
			name: "h1 is the only headline",
			body: `<html><head><title>关于信息系统维护的通知</title></head><body><h1>关于信息系统维护的通知</h1></body></html>`,
			want: "关于信息系统维护的通知",
		},
		{
			name: "site-name h1 alone is not a headline",
			body: `<html><head><title>通知-成都大学信息网络中心</title></head><body><h1>成都大学信息网络中心</h1></body></html>`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := detailMetadataInfo(parseFixture(t, tc.body))
			if info.Title != tc.want {
				t.Fatalf("wanted title %q, got %q", tc.want, info.Title)
			}
		})
	}
	// The real journal detail page keeps its .art-tit headline.
	body, err := os.ReadFile("../../research/journal-detail.html")
	if err != nil {
		t.Fatal(err)
	}
	info := detailMetadataInfo(parseFixture(t, string(body)))
	if !strings.Contains(info.Title, "开展组稿约稿与期刊宣传") || info.Date != "2026-09-14" {
		t.Errorf("journal detail metadata regressed: %+v", info)
	}
}

// Breadcrumb, copyright and footer blocks must not contribute articles at all.
func TestDocumentChromeIsExcludedFromArticles(t *testing.T) {
	body := `<div class="position"><a href="info/1/9.htm">当前位置：首页 &gt; 通知</a><span>2026-07-14</span></div>
<div class="breadcrumb"><a href="info/1/8.htm">您现在的位置：首页</a><span>2026-07-14</span></div>
<div class="copyright"><a href="info/1/7.htm">版权所有 成都大学</a><span>2026-07-14</span></div>
<div class="footer"><a href="info/1/6.htm">主办单位：成都大学</a><span>2026-07-14</span></div>
<ul><li><a href="info/1/5.htm">关于开展消防演练的通知</a><span>2026-07-14</span></li></ul>`
	items := parseArticles(parseFixture(t, body), "https://x.cdu.edu.cn/", model.Source{ID: "s1"}, "通知公告", "")
	if len(items) != 1 || !strings.HasSuffix(items[0].URL, "/info/1/5.htm") {
		t.Fatalf("document chrome leaked into articles: %+v", items)
	}
}

// The 「发布时间/发布日期」 marker and the VSB 「时间：… 浏览量：」 metadata line are
// the only signals allowed to replace a list date. A 「时间：…」 line inside the
// article body describes an event, so it must not be marked explicit.
func TestDetailDateExplicitOnlyForPublicationMetadata(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantDate     string
		wantExplicit bool
	}{
		{
			name: "explicit 发布时间 on the lib trial-database page",
			body: `<body><div class="infotitle">古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）</div>
<div class="bodycon"><div class="newsinfo">来源： 发布时间：2022-05-16 编辑：技术部 浏览数：</div></div>
<div id="vsb_content" class="bodycon"><div class="v_news_content"><p>试用期限：即日起--2026年12月31日</p></div></div></body>`,
			wantDate: "2022-05-16", wantExplicit: true,
		},
		{
			name:     "VSB 时间/浏览量 headline line",
			body:     `<body><div class="art-tit cont-tit"><h3>期刊中心参加学术会议</h3><p class="rq"><span>时间：2026-09-14 浏览量：32</span></p></div><div id="vsb_content" class="art-body"><p>正文</p></div></body>`,
			wantDate: "2026-09-14", wantExplicit: true,
		},
		{
			name:     "event time inside the article body",
			body:     `<body><div class="art-tit cont-tit"><h3>关于举办培训的通知</h3></div><div id="vsb_content" class="art-body"><div class="v_news_content"><p>时间：2026-11-13 浏览：120</p></div></div></body>`,
			wantDate: "2026-11-13", wantExplicit: false,
		},
		{
			name:     "date only in the headline",
			body:     `<body><div class="infotitle">某平台（试用有效期：即日起--2026年12月31日）</div></body>`,
			wantDate: "", wantExplicit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := detailMetadataInfo(parseFixture(t, tc.body))
			if info.Date != tc.wantDate || info.DateExplicit != tc.wantExplicit {
				t.Fatalf("wanted (%q,%v), got (%q,%v)", tc.wantDate, tc.wantExplicit, info.Date, info.DateExplicit)
			}
		})
	}
	// The real journal detail page prints 「时间：… 浏览量：」 inside .art-tit.
	body, err := os.ReadFile("../../research/journal-detail.html")
	if err != nil {
		t.Fatal(err)
	}
	if info := detailMetadataInfo(parseFixture(t, string(body))); !info.DateExplicit {
		t.Errorf("journal detail publication line not treated as explicit: %+v", info)
	}
}

// detailCandidates must cover a whole column and put the records that gain the
// most first, but never exceed the budget.
func TestDetailCandidatesOrdering(t *testing.T) {
	articles := make([]model.Article, 0, 60)
	for i := 0; i < 60; i++ {
		articles = append(articles, model.Article{Title: fmt.Sprintf("第%d条完整标题", i), Date: "2026-09-14"})
	}
	articles[0].Title = "标题被截断的公告..."
	articles[1].Date = ""
	articles[2].Date = ""

	indices := detailCandidates(articles)
	if len(indices) != detailBudget {
		t.Fatalf("wanted %d candidates, got %d", detailBudget, len(indices))
	}
	if indices[0] != 0 || indices[1] != 1 || indices[2] != 2 {
		t.Errorf("truncated and undated records must come first: %v", indices[:3])
	}
	if detailBudget < 20 {
		t.Errorf("detail budget %d cannot cover a normal column", detailBudget)
	}
	// A small source is capped by its own article count.
	short := detailCandidates(articles[:5])
	if len(short) != 5 {
		t.Errorf("budget must not exceed the article count: %d", len(short))
	}
}

// CrawlSource must fill in every missing date of a column (the old budget of 12
// left the tail undated) and may only replace a list date with explicit
// publication metadata.
func TestCrawlFillsDatesAndOnlyExplicitDetailsOverride(t *testing.T) {
	pages := fixtureTransport{
		"https://test.cdu.edu.cn/": `<li><a href="tzgg.htm">通知公告</a></li>`,
	}
	list := &strings.Builder{}
	// 20 undated rows: every one of them must be enriched by its detail page, which
	// is what the old budget of 12 could not do.
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(list, `<li><a href="info/3/%d.htm">未标注日期的通知标题第%d条</a><span class="date"></span></li>`, i, i)
		pages[fmt.Sprintf("https://test.cdu.edu.cn/info/3/%d.htm", i)] = fmt.Sprintf(`<div class="newsinfo">发布时间：2026-09-%02d 编辑：技术部</div>`, i)
	}
	// A wrong list date must be corrected by the explicit 发布时间.
	list.WriteString(`<li><a href="info/3/21.htm">列表日期错误的科研公告</a><span class="date">2026-09-01</span></li>`)
	// A correct list date must survive a detail page without any date.
	list.WriteString(`<li><a href="info/3/22.htm">列表日期正确的会议通知</a><span class="date">2026-09-05</span></li>`)
	// A deadline inside the headline is not publication metadata.
	list.WriteString(`<li><a href="info/3/23.htm">平台试用通知</a><span class="date">2026-09-06</span></li>`)
	// A truncated list headline is restored from the detail page.
	list.WriteString(`<li><a href="info/3/24.htm">详情页可以补全的完整标题...</a><span class="date">2026-09-07</span></li>`)
	list.WriteString(`</ul>`)
	pages["https://test.cdu.edu.cn/tzgg.htm"] = list.String()
	pages["https://test.cdu.edu.cn/info/3/21.htm"] = `<div class="newsinfo">来源： 发布时间：2022-05-16 编辑：技术部</div>`
	pages["https://test.cdu.edu.cn/info/3/22.htm"] = `<div class="content"><p>正文没有日期</p></div>`
	pages["https://test.cdu.edu.cn/info/3/23.htm"] = `<div class="infotitle">某平台（试用有效期：即日起--2026年12月31日）</div>`
	pages["https://test.cdu.edu.cn/info/3/24.htm"] = `<div class="content"><p>正文没有日期</p></div><title>x</title>`

	c := New()
	c.interval = 0
	c.client.Transport = pages
	source, articles, err := c.CrawlSource(context.Background(), model.Source{ID: "test", Name: "测试部门", URL: "https://test.cdu.edu.cn/", Kind: "department"}, model.SyncOptions{})
	if err != nil {
		t.Fatalf("unexpected warning: %v", err)
	}
	if source.Status != "success" || len(articles) != 24 {
		t.Fatalf("wrong crawl result: %s / %d articles", source.Status, len(articles))
	}
	byURL := map[string]model.Article{}
	for _, article := range articles {
		byURL[article.URL] = article
	}
	for i := 1; i <= 20; i++ {
		url := fmt.Sprintf("https://test.cdu.edu.cn/info/3/%d.htm", i)
		article, ok := byURL[url]
		if !ok {
			t.Fatalf("missing article %s", url)
		}
		if want := fmt.Sprintf("2026-09-%02d", i); article.Date != want {
			t.Errorf("%s: wanted date %s, got %+v", url, want, article)
		}
	}
	// 详情页显式的「发布时间」覆盖列表页的错值。
	if got := byURL["https://test.cdu.edu.cn/info/3/21.htm"]; got.Date != "2022-05-16" || got.DateText != "2022-05-16" {
		t.Errorf("explicit detail date did not override the list date: %+v", got)
	}
	// 详情页取不到日期时保留列表值。
	if got := byURL["https://test.cdu.edu.cn/info/3/22.htm"]; got.Date != "2026-09-05" {
		t.Errorf("list date was lost when the detail page had none: %+v", got)
	}
	// 只有标题里的有效期时同样保留列表值。
	if got := byURL["https://test.cdu.edu.cn/info/3/23.htm"]; got.Date != "2026-09-06" {
		t.Errorf("headline deadline replaced the list date: %+v", got)
	}
}

// The list date must also survive when the detail page cannot be fetched: the
// extra correction fetches are best effort and must not degrade a source.
func TestUnreachableDetailKeepsListDateAndStatus(t *testing.T) {
	c := New()
	c.interval = 0
	c.client.Transport = fixtureTransport{
		"https://test.cdu.edu.cn/":         `<li><a href="tzgg.htm">通知公告</a></li>`,
		"https://test.cdu.edu.cn/tzgg.htm": `<li><a href="info/4/1.htm">列表日期正确的通知标题</a><span class="date">2026-09-12</span></li>`,
	}
	source, articles, err := c.CrawlSource(context.Background(), model.Source{ID: "test", Name: "测试部门", URL: "https://test.cdu.edu.cn/", Kind: "department"}, model.SyncOptions{})
	if len(articles) != 1 || articles[0].Date != "2026-09-12" {
		t.Fatalf("list date not preserved: %+v", articles)
	}
	if source.Status != "success" || err != nil {
		t.Errorf("best-effort detail fetch degraded the source: %s / %v", source.Status, err)
	}
}
