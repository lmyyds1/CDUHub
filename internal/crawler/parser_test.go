package crawler

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cduhub/internal/model"
	"golang.org/x/net/html"
)

func parseFixture(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestOfficialDirectoryIncludesNestedAndUnavailableUnits(t *testing.T) {
	for _, tc := range []struct {
		file, kind string
		count      int
	}{{"zzjg-directory.html", "department", 43}, {"yxsz-directory.html", "college", 30}} {
		body, err := os.ReadFile("testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		sources := parseDirectory(parseFixture(t, string(body)), "https://www.cdu.edu.cn/"+tc.file, tc.kind)
		if len(sources) != tc.count {
			t.Fatalf("%s: wanted %d sources, got %d", tc.kind, tc.count, len(sources))
		}
		names := map[string]model.Source{}
		for _, source := range sources {
			names[source.Name] = source
			if source.Name == "学生" || source.Name == "统一信息门户" || source.Name == "教职工" {
				t.Fatalf("header/footer leaked: %+v", source)
			}
		}
		if tc.kind == "college" {
			for _, name := range []string{"基础医学院", "护理学院", "美术与设计学院", "影视与动画学院", "音乐与舞蹈学院", "国际部"} {
				if _, ok := names[name]; !ok {
					t.Errorf("missing nested unit %s", name)
				}
			}
			if names["基础医学院"].ID == names["护理学院"].ID {
				t.Error("same-site colleges must retain separate identities")
			}
			if names["化学化工学院"].Status != "unavailable" || names["化学化工学院"].URL != "" {
				t.Error("placeholder link must remain visibly unavailable")
			}
		} else if names["体育场馆中心"].Status != "unavailable" {
			t.Error("missing unavailable sports venue")
		}
	}
}

func TestArticleDatesAndTitlesStayAttachedToTheirRows(t *testing.T) {
	body := `<section><h2>通知公告</h2><ul>
<li><a href="info/1001/2001.htm" title="关于2026年9月1日活动报名的完整通知">关于报名...</a><span class="date">2026-09-14</span></li>
<li><a href="info/1001/2002.htm">仅提供月日的通知公告</a><span>09-03</span></li>
<li><a href="info/1001/2003.htm">没有发布日期的通知公告</a></li>
<li><a href="info/1001/2004.htm">学院分块日期的通知公告</a><div class="date"><b>12</b><span>2026.08</span></div></li>
<li><a href="/tzgg.htm">通知公告</a></li></ul></section>`
	items := parseArticles(parseFixture(t, body), "https://xzbgs.cdu.edu.cn/", model.Source{ID: "s1", Name: "学校办公室", Kind: "department"}, "通知公告", "2026-09-14T00:00:00Z")
	if len(items) != 4 {
		t.Fatalf("wanted 4 articles; got %+v", items)
	}
	if items[0].Title != "关于2026年9月1日活动报名的完整通知" || items[0].Date != "2026-09-14" {
		t.Errorf("wrong full title or publication date: %+v", items[0])
	}
	if items[1].Date != "" || items[1].DateText != "09-03" {
		t.Errorf("year was invented: %+v", items[1])
	}
	if items[2].Date != "" || items[2].DateText != "" {
		t.Errorf("neighbor date leaked into undated article: %+v", items[2])
	}
	if items[3].Date != "2026-08-12" {
		t.Errorf("split date not parsed: %+v", items[3])
	}
}

func TestChannelsMoreLinksAndPagination(t *testing.T) {
	body := `<nav><a href="/tzgg.htm">通知公告</a><a href="/ywlc.htm">业务流程</a><a href="/dqgz.htm">党务工作</a><a href="/xwgz.htm">校务工作</a><a href="https://oa.cdu.edu.cn">办公OA系统</a></nav>
<section><div class="title"><span>领导动态</span><a href="/lddt.htm">MORE</a></div><ul><li><a href="info/1/2.htm">学校领导动态的长篇新闻标题</a><span>09-14</span></li></ul></section>
<div class="p_pages"><a href="tzgg/9.htm">下一页</a><a href="tzgg/1.htm">尾页</a></div>
<footer><a href="https://jw.cdu.edu.cn/tzgg.htm">教务通知</a></footer>`
	doc := parseFixture(t, body)
	channels := parseChannels(doc, "https://xzbgs.cdu.edu.cn/", "s1")
	if len(channels) != 5 {
		t.Fatalf("wanted 5 channels; got %+v", channels)
	}
	if channels[0].Name != "通知公告" {
		t.Error("notices should be prioritized")
	}
	if next := nextPage(doc, "https://xzbgs.cdu.edu.cn/tzgg.htm"); next != "https://xzbgs.cdu.edu.cn/tzgg/9.htm" {
		t.Errorf("wrong VSB next page: %s", next)
	}
}

func TestDetailPrefersPublicationMetadata(t *testing.T) {
	body := `<html><head><meta name="ArticleTitle" content="完整的新闻公告标题"><meta name="PubDate" content="2026-09-03 08:01:00"></head><body><h1>截短标题...</h1><article>活动举办于2026年10月1日。</article></body></html>`
	title, date, _ := detailMetadata(parseFixture(t, body))
	if title != "完整的新闻公告标题" || date != "2026-09-03" {
		t.Fatalf("wrong metadata: %q %q", title, date)
	}
	_, date, _ = detailMetadata(parseFixture(t, `<body><article>活动举办于2026年10月1日。</article><footer>2025-12-31</footer></body>`))
	if date != "" {
		t.Fatalf("body event date should not be publication date: %q", date)
	}
}

func TestOfficialDomainAndAuthenticationBoundaries(t *testing.T) {
	for _, raw := range []string{"http://evil.cdu.edu.cn.attacker.test/", "https://cdu.edu.cn@attacker.test/", "https://evil-cdu.edu.cn/", "file:///tmp/test", "https://www.cdu.edu.cn:8000/"} {
		if officialURL(raw) {
			t.Errorf("accepted unsafe URL %q", raw)
		}
	}
	if resolveURL("https://xzbgs.cdu.edu.cn/", "//www.cdu.edu.cn/zzjg.htm#section") != DepartmentDirectory {
		t.Error("bad protocol-relative URL")
	}
	if resolveURL(DepartmentDirectory, "#") != "" {
		t.Error("placeholder should not resolve as directory")
	}
	if !protectedURL("https://id.cdu.edu.cn/login") {
		t.Error("CAS endpoint not detected")
	}
	c := New()
	req, _ := http.NewRequest("GET", "https://attacker.test/", nil)
	if c.client.CheckRedirect(req, nil) == nil {
		t.Error("cross-domain redirect should be refused")
	}
}

type fixtureTransport map[string]string

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := f[req.URL.String()]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
		body = "not found"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestCrawlFollowsNestedColumnsAndPageBudget(t *testing.T) {
	c := New()
	c.interval = 0
	c.client.Transport = fixtureTransport{
		"https://test.cdu.edu.cn/":             `<nav><a href="news.htm">新闻动态</a></nav>`,
		"https://test.cdu.edu.cn/news.htm":     `<a href="notice.htm">通知公告</a><li><a href="info/1/1.htm">新闻第一篇完整标题</a><span>2026-09-14</span></li><a href="news/2.htm">下一页</a>`,
		"https://test.cdu.edu.cn/news/2.htm":   `<li><a href="../info/1/2.htm">新闻第二页完整标题</a><span>2026-09-13</span></li><a href="1.htm">下一页</a>`,
		"https://test.cdu.edu.cn/notice.htm":   `<li><a href="info/2/1.htm" title="通知公告详情完整标题">通知公告...</a><span>09-12</span></li>`,
		"https://test.cdu.edu.cn/info/2/1.htm": `<meta name="PubDate" content="2026-09-12"><meta name="ArticleTitle" content="通知公告详情完整标题">`,
	}
	source, articles, err := c.CrawlSource(context.Background(), model.Source{ID: "test", Name: "测试部门", URL: "https://test.cdu.edu.cn/", Kind: "department"}, model.SyncOptions{PagesPerChannel: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(articles) != 3 || len(source.Channels) != 2 {
		t.Fatalf("wrong crawl result: %+v %+v", source, articles)
	}
	if articles[2].Date != "2026-09-12" {
		t.Errorf("detail date enrichment failed: %+v", articles[2])
	}
	if source.Status != "success" {
		t.Error(source.Status)
	}
}

func TestCancellationDoesNotWaitForRateLimit(t *testing.T) {
	c := New()
	c.nextRequest["test.cdu.edu.cn"] = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _, err := c.fetch(ctx, "https://test.cdu.edu.cn/")
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancelled request waited for rate limit")
	}
}

func TestRealSiteColumnAndArticleRegressions(t *testing.T) {
	for _, tc := range []struct{ file, base, channel, channelName string }{
		{"oice.html", "https://oice.cdu.edu.cn/", "xwgg/tzgg.htm", "通知公告"},
		{"nic.html", "https://nic.cdu.edu.cn/", "index/tzgg.htm", "通知公告"},
		{"jw.html", "https://jw.cdu.edu.cn/", "jwgg.htm", "通知公告"},
		{"xzbgs.html", "https://xzbgs.cdu.edu.cn/", "tzgg1.htm", "通知通告"},
	} {
		body, err := os.ReadFile("../../research/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		channels := parseChannels(parseFixture(t, string(body)), tc.base, "source")
		found := false
		for _, channel := range channels {
			if strings.Contains(channel.Name, "更多") || strings.Contains(channel.URL, "%7B") {
				t.Errorf("%s: bad channel %+v", tc.file, channel)
			}
			if channel.URL == tc.base+tc.channel {
				found = true
				if channel.Name != tc.channelName {
					t.Errorf("%s: incorrect notice label %+v", tc.file, channel)
				}
			}
		}
		if !found {
			t.Errorf("%s: notice channel absent from %+v", tc.file, channels)
		}
	}
	body, err := os.ReadFile("../../research/journal.html")
	if err != nil {
		t.Fatal(err)
	}
	articles := parseArticles(parseFixture(t, string(body)), "https://journal.cdu.edu.cn/", model.Source{ID: "journal"}, "新闻动态", "")
	journalFound := false
	for _, article := range articles {
		if strings.HasSuffix(article.URL, "/1463.htm") {
			journalFound = true
			if strings.Contains(article.Title, "由全国") || strings.Contains(article.Title, "2026-09-14") || len([]rune(article.Title)) > 55 {
				t.Errorf("summary leaked into title: %+v", article)
			}
			if article.Date != "2026-09-14" {
				t.Errorf("wrong journal date: %+v", article)
			}
		}
	}
	if !journalFound {
		t.Error("journal headline missing")
	}
	body, err = os.ReadFile("../../research/journal-detail.html")
	if err != nil {
		t.Fatal(err)
	}
	title, date, _ := detailMetadata(parseFixture(t, string(body)))
	if !strings.Contains(title, "开展组稿约稿与期刊宣传") || date != "2026-09-14" {
		t.Errorf("journal detail metadata not found: %s / %s", title, date)
	}
	body, err = os.ReadFile("../../research/pharm.html")
	if err != nil {
		t.Fatal(err)
	}
	articles = parseArticles(parseFixture(t, string(body)), "https://pharm.cdu.edu.cn/", model.Source{ID: "pharm"}, "新闻动态", "")
	crossHost := false
	for _, article := range articles {
		if article.URL == "https://news.cdu.edu.cn/info/1002/49516.htm" {
			crossHost = true
			if article.Date != "2026-09-10" || strings.Contains(article.Title, "2026-09-10") {
				t.Errorf("incorrect cross-site article %+v", article)
			}
		}
	}
	if !crossHost {
		t.Error("official cross-subdomain article was dropped")
	}
}

func TestMechanicalCollegeDivHeadlineExcludesSummary(t *testing.T) {
	body, err := os.ReadFile("testdata/jxgc-list.html")
	if err != nil {
		t.Fatal(err)
	}
	items := parseArticles(parseFixture(t, string(body)), "https://jxgc.cdu.edu.cn/dq/llxx.htm", model.Source{ID: "jxgc"}, "理论学习", "")
	if len(items) != 10 {
		t.Fatalf("expected 10 real list articles, got %d", len(items))
	}
	if items[0].Title != "机械工程学院组织全院教职工开展政治理论学习" || items[0].Date != "2026-07-20" {
		t.Fatalf("summary/date leaked into title: %+v", items[0])
	}
	for _, item := range items {
		if len([]rune(item.Title)) > 100 {
			t.Errorf("unexpected long mechanical headline: %+v", item)
		}
	}
}

func TestShortFacultyHeadingDoesNotFallBackToBiography(t *testing.T) {
	body, err := os.ReadFile("../../research/pharm.html")
	if err != nil {
		t.Fatal(err)
	}
	items := parseArticles(parseFixture(t, string(body)), "https://pharm.cdu.edu.cn/", model.Source{ID: "pharm"}, "首页动态", "")
	want := map[string]string{"4346.htm": "任静", "4329.htm": "郭晓强", "4337.htm": "褚以文"}
	for _, item := range items {
		for suffix, name := range want {
			if strings.HasSuffix(item.URL, "/"+suffix) {
				if item.Title != name {
					t.Errorf("profile name included biography: %+v", item)
				}
				delete(want, suffix)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("faculty profile titles missing: %+v", want)
	}
}

func TestTitleStructurePreservesLegitimateLongHeadline(t *testing.T) {
	longTitle := "【成大讲坛】P. Andrew. Evans：Enantioselective Allylic Cross-Coupling Reactions for the Construction of Acyclic Tertiary and Quaternary Stereocenters"
	body := `<li><a href="info/1141/2613.htm"><div class="title-wrap"><h3>` + longTitle + `</h3><p class="summary">A long introductory summary that must not be part of the headline.</p></div><time>2026-06-04</time></a></li>`
	items := parseArticles(parseFixture(t, body), "https://lib.cdu.edu.cn/", model.Source{ID: "lib"}, "资讯动态", "")
	if len(items) != 1 || items[0].Title != longTitle {
		t.Fatalf("legitimate scientific title was shortened or polluted: %+v", items)
	}
}

// A trial-database page prints its validity period in the headline while the
// real publication date sits in a sibling block below it. lib.cdu.edu.cn/info/1057/1571.htm
// was recorded as 2026-12-31 instead of 2022-05-16 because the headline container
// matched the old "class contains info" check first.
func TestDetailPrefersPublicationDateOverHeadlineValidityPeriod(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantDate, wantOriginal string
	}{
		{
			name: "headline container before news metadata",
			body: `<body><div class="infotitle">古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）</div>
<div class="bodycon"><div class="newsinfo">来源： 发布时间：2022-05-16 编辑：技术部 浏览数：</div></div>
<div id="vsb_content" class="bodycon"><div class="v_news_content"><p>试用期限：即日起--2026年12月31日</p></div></div></body>`,
			wantDate: "2022-05-16", wantOriginal: "2022-05-16",
		},
		{
			name: "headline and deadline in one container",
			body: `<body><div class="pagew"><div class="infotitle">Mirror（米乐）心理资源服务平台（试用有效期：即日起--2026年12月31日）</div>
<div class="newsinfo">发布时间：2022-03-01 编辑：钟涛</div></div></body>`,
			wantDate: "2022-03-01", wantOriginal: "2022-03-01",
		},
		{
			name:     "only a validity period is shown",
			body:     `<body><div class="infotitle">广益多媒体外语学习平台（试用有效期：即日起--2026年3月1日）</div></body>`,
			wantDate: "", wantOriginal: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, date, original := detailMetadata(parseFixture(t, tc.body))
			if date != tc.wantDate || original != tc.wantOriginal {
				t.Fatalf("wanted (%q,%q), got (%q,%q)", tc.wantDate, tc.wantOriginal, date, original)
			}
		})
	}
}

// journal.cdu.edu.cn renders three policy documents side by side. None of them
// shows a publication date, so the old 5-level ancestor walk stole the first
// date it found in a neighbouring row's summary and reported 1998-04-01.
func TestListDateDoesNotLeakFromSiblingRows(t *testing.T) {
	body := `<div class="s4-main"><div>
<div class="s4-con"><a href="info/1058/1332.htm"><h4>《期刊出版管理规定》</h4><p class="overs eclip2">新闻出版总署令第31号发布，2005年12月1日施行。</p></a></div>
<div class="s4-con"><a href="info/1058/1333.htm"><h4>《高等学校学报管理办法》</h4><p class="overs eclip2">教育部1998年4月1日发布，1998年5月1日起实行。</p></a></div>
<div class="s4-con"><a href="info/1058/1334.htm"><h4>关于进一步加强和改进高校出版工作的意见</h4><p class="overs eclip2">各省、自治区、直辖市教育厅（教委）：</p></a></div>
</div></div>`
	items := parseArticles(parseFixture(t, body), "https://journal.cdu.edu.cn/", model.Source{ID: "journal"}, "政策法规", "")
	if len(items) != 3 {
		t.Fatalf("wanted 3 policy documents, got %d", len(items))
	}
	for _, item := range items {
		if item.Date != "" || item.DateText != "" {
			t.Errorf("sibling summary date leaked into %s: %+v", item.URL, item)
		}
	}
}

// A row that prints an event date in its summary and the publication date in a
// separate span must report the span, not the event date.
func TestRowSummaryEventDateIsNotUsedAsPublicationDate(t *testing.T) {
	body := `<ul><li><a href="info/1046/12227.htm">“2026年成都大学科研成果转化与技术创新国际会议”会议服务项目比选公告</a>
<div class="txt-right-p">现将有关事项公告如下，会议将于2026年11月13日在成都大学举办。</div><span class="date">2026-09-24</span></li></ul>`
	items := parseArticles(parseFixture(t, body), "https://jxgc.cdu.edu.cn/", model.Source{ID: "jxgc"}, "通知公告", "")
	if len(items) != 1 {
		t.Fatalf("wanted 1 article, got %d", len(items))
	}
	if items[0].Date != "2026-09-24" {
		t.Errorf("event date was used as publication date: %+v", items[0])
	}
}

// A split-block date span is parsed correctly, but some templates repeat the
// same date as ISO text inside the headline. Only the duplicate is removed.
func TestHeadlineEdgeDateDuplicateIsTrimmed(t *testing.T) {
	body := `<ul><li><a href="info/1046/12201.htm">成都大学科学技术项目协作外拨事项公示 2026-09-14</a>
<div class="pull-left txt-date text-center"><span>14</span><br>2026-09</div></li></ul>`
	items := parseArticles(parseFixture(t, body), "https://jxgc.cdu.edu.cn/", model.Source{ID: "jxgc"}, "通知公告", "")
	if len(items) != 1 {
		t.Fatalf("wanted 1 article, got %d", len(items))
	}
	if items[0].Date != "2026-09-14" {
		t.Errorf("split date not parsed: %+v", items[0])
	}
	if items[0].Title != "成都大学科学技术项目协作外拨事项公示" {
		t.Errorf("duplicate headline date not trimmed: %q", items[0].Title)
	}
	// A date that is part of the headline itself must survive.
	body = `<ul><li><a href="info/1046/12000.htm">关于2026年9月14日开展消防演练的通知</a><span class="date">2026-09-10</span></li></ul>`
	items = parseArticles(parseFixture(t, body), "https://jxgc.cdu.edu.cn/", model.Source{ID: "jxgc"}, "通知公告", "")
	if len(items) != 1 || items[0].Title != "关于2026年9月14日开展消防演练的通知" {
		t.Errorf("headline date inside the title was removed: %+v", items)
	}
}

// jxgc 首页 puts the publication date inside the article anchor with no markup
// of its own, so it used to become part of the headline while the record was
// left without a date at all.
func TestDateInsideArticleAnchorBecomesPublicationDate(t *testing.T) {
	body := `<ul><li><a class="txt-elise" href="info/1047/12208.htm">【迎新工作】机械工程学院圆满完成2026级迎新工作<span>2026-09-15</span></a></li></ul>`
	items := parseArticles(parseFixture(t, body), "https://jxgc.cdu.edu.cn/", model.Source{ID: "jxgc"}, "首页动态", "")
	if len(items) != 1 {
		t.Fatalf("wanted 1 article, got %d", len(items))
	}
	if items[0].Date != "2026-09-15" || items[0].DateText != "2026-09-15" {
		t.Errorf("date inside the anchor was not recovered: %+v", items[0])
	}
	if items[0].Title != "【迎新工作】机械工程学院圆满完成2026级迎新工作" {
		t.Errorf("anchor date was left in the headline: %q", items[0].Title)
	}
	// A headline whose trailing number is not a calendar date stays untouched.
	body = `<ul><li><a href="info/1047/12209.htm">关于2026级新生报到的通知 2026-13-45</a></li></ul>`
	items = parseArticles(parseFixture(t, body), "https://jxgc.cdu.edu.cn/", model.Source{ID: "jxgc"}, "首页动态", "")
	if len(items) != 1 || items[0].Date != "" || !strings.HasSuffix(items[0].Title, "2026-13-45") {
		t.Errorf("invalid trailing date was treated as a publication date: %+v", items)
	}
}

// bs.cdu.edu.cn wraps the headline and the date in one anchor, the date living
// in a bare <span> with no class of its own.
func TestBareDateElementInsideAnchorIsPublicationDate(t *testing.T) {
	body := `<div class="list"><a href="info/1110/13266.htm"><p>西南财经大学-罗格斯大学-成都大学会计联合研讨会成功举办</p><span>2026-10-19</span><div class="clear"></div></a></div>`
	items := parseArticles(parseFixture(t, body), "https://bs.cdu.edu.cn/", model.Source{ID: "bs"}, "首页动态", "")
	if len(items) != 1 {
		t.Fatalf("wanted 1 article, got %d", len(items))
	}
	if items[0].Date != "2026-10-19" {
		t.Errorf("bare date element not used as publication date: %+v", items[0])
	}
	if strings.Contains(items[0].Title, "2026-10-19") {
		t.Errorf("date leaked into the headline: %q", items[0].Title)
	}
}

// A row whose only date-like text is a deadline keeps an empty date instead of
// reporting the deadline as the publication date.
func TestDeadlineOnlyRowKeepsNoDate(t *testing.T) {
	body := `<ul><li><a href="info/1012/1473.htm">关于魔方数独协会“魔方进小区”志愿活动的通知</a>
<div class="desc">活动将于2023年10月28日14:00至17:00在书香邻里书屋开展，报名截止2023年10月26日。</div></li></ul>`
	items := parseArticles(parseFixture(t, body), "https://sqjsb.cdu.edu.cn/", model.Source{ID: "sqjsb"}, "通知公告", "")
	if len(items) != 1 {
		t.Fatalf("wanted 1 article, got %d", len(items))
	}
	if items[0].Date != "" {
		t.Errorf("deadline was used as publication date: %+v", items[0])
	}
}
