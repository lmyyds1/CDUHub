package crawler

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"cduhub/internal/model"
	"golang.org/x/net/html"
)

var (
	fullDateRE    = regexp.MustCompile(`(?:19|20)\d{2}\s*[-/.年]\s*\d{1,2}\s*[-/.月]\s*\d{1,2}日?`)
	datePartsRE   = regexp.MustCompile(`\d+`)
	partialDateRE = regexp.MustCompile(`(?:^|[^\d])((?:0?[1-9]|1[0-2])\s*[-/.月]\s*(?:0?[1-9]|[12]\d|3[01])日?)(?:[^\d]|$)`)
	splitDateRE   = regexp.MustCompile(`(?:^|\s)(\d{1,2})\s+((?:19|20)\d{2})\s*[-/.年]\s*(\d{1,2})(?:月|\s|$)`)
	articlePathRE = regexp.MustCompile(`(?i)(/info/\d+/\d+\.(s?html?|jsp)|/(?:content|article|newsdetail|show)[/_-]|/(?:content|article|detail|show)\.(?:html?|php|aspx|jsp))`)
	pageNumberRE  = regexp.MustCompile(`^\d+$`)
	spaceRE       = regexp.MustCompile(`\s+`)
	// A date at the very end of a headline, with no markup separating it (some
	// list templates place the publication date inside the article anchor).
	trailingDateRE = regexp.MustCompile(`(?:19|20)\d{2}\s*[-/.年]\s*\d{1,2}\s*[-/.月]\s*\d{1,2}日?$`)
)

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum[:10])
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	for _, value := range strings.Fields(attr(n, "class")) {
		if value == class {
			return true
		}
	}
	return false
}

func walk(n *html.Node, visit func(*html.Node) bool) {
	if n == nil || !visit(n) {
		return
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func textContent(n *html.Node) string {
	var b strings.Builder
	walk(n, func(v *html.Node) bool {
		if v.Type == html.ElementNode && (v.Data == "script" || v.Data == "style" || v.Data == "noscript") {
			return false
		}
		if v.Type == html.TextNode {
			b.WriteString(v.Data)
			b.WriteByte(' ')
		}
		return true
	})
	return clean(b.String())
}

func clean(s string) string {
	return strings.TrimSpace(spaceRE.ReplaceAllString(strings.ReplaceAll(s, "\u00a0", " "), " "))
}

func officialURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return (host == "cdu.edu.cn" || strings.HasSuffix(host, ".cdu.edu.cn")) && (u.Port() == "" || u.Port() == "80" || u.Port() == "443")
}

func resolveURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") || strings.ContainsAny(ref, "{}<>\"\r\n") || strings.HasPrefix(strings.ToLower(ref), "href=") {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u, err := b.Parse(ref)
	if err != nil {
		return ""
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	if !officialURL(u.String()) {
		return ""
	}
	return u.String()
}

func sameSite(a, b string) bool {
	au, e1 := url.Parse(a)
	bu, e2 := url.Parse(b)
	return e1 == nil && e2 == nil && strings.EqualFold(au.Hostname(), bu.Hostname())
}

func protectedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, prefix := range []string{"id.", "vpn.", "myapp.", "oa.", "ehall.", "mail."} {
		if strings.HasPrefix(host, prefix) {
			return true
		}
	}
	p := strings.ToLower(u.Path)
	return strings.Contains(p, "/login") || strings.Contains(p, "/cas/") || strings.Contains(p, "/auth/")
}

func parseDirectory(doc *html.Node, base, kind string) []model.Source {
	var sources []model.Source
	seen := map[string]bool{}
	walk(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode || !hasClass(n, "box") {
			return true
		}
		insideDirectory := false
		for parent := n.Parent; parent != nil; parent = parent.Parent {
			if hasClass(parent, "n_container") && hasClass(parent, "znbm") {
				insideDirectory = true
				break
			}
		}
		if !insideDirectory {
			return false
		}
		var group string
		walk(n, func(v *html.Node) bool {
			if hasClass(v, "line") {
				group = textContent(v)
				return false
			}
			return true
		})
		walk(n, func(v *html.Node) bool {
			if v.Type != html.ElementNode || v.Data != "a" || (!hasClass(v, "a") && !hasClass(v, "a1")) {
				return true
			}
			name := clean(attr(v, "title"))
			if name == "" {
				name = textContent(v)
			}
			if name == "" {
				return false
			}
			raw := resolveURL(base, attr(v, "href"))
			key := kind + "|" + name + "|" + raw
			if seen[key] {
				return false
			}
			seen[key] = true
			source := model.Source{ID: stableID(kind, name, raw), Name: name, URL: raw, Kind: kind, Group: group, Status: "pending", Channels: []model.Channel{}}
			if raw == "" {
				source.Status = "unavailable"
				source.Error = "官网目录未提供可访问的网址"
			}
			sources = append(sources, source)
			return false
		})
		return false
	})
	return sources
}

func dateValue(raw string) (string, string) {
	if found := fullDateRE.FindString(raw); found != "" {
		parts := datePartsRE.FindAllString(found, -1)
		if len(parts) >= 3 {
			y, _ := strconv.Atoi(parts[0])
			m, _ := strconv.Atoi(parts[1])
			d, _ := strconv.Atoi(parts[2])
			if validDate(y, m, d) {
				return fmt.Sprintf("%04d-%02d-%02d", y, m, d), found
			}
		}
	}
	if parts := splitDateRE.FindStringSubmatch(raw); len(parts) == 4 {
		d, _ := strconv.Atoi(parts[1])
		y, _ := strconv.Atoi(parts[2])
		m, _ := strconv.Atoi(parts[3])
		if validDate(y, m, d) {
			return fmt.Sprintf("%04d-%02d-%02d", y, m, d), clean(parts[0])
		}
	}
	if found := partialDateRE.FindStringSubmatch(raw); len(found) > 1 {
		return "", clean(found[1])
	}
	return "", ""
}

func validDate(y, m, d int) bool {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Year() == y && int(t.Month()) == m && t.Day() == d
}

// Publication-date vocabulary. A page often shows several dates: the publication
// date, an application deadline inside the title, and event dates in the summary.
// Only the first group describes when the article was published.
var (
	publishDateWords = []string{"发布时间", "发布日期", "发表时间", "发布于", "publish", "pubdate", "published"}
	// Context that marks a date as a deadline/event date rather than a publication
	// date. A title such as 「…（试用有效期：即日起--2026年12月31日）」 must not
	// replace the real publication date.
	nonPublishDateWords = []string{"有效期", "截止", "将于", "举办", "报名", "考试", "开始时间", "结束时间", "开馆", "闭馆"}
)

func containsAnyWord(text string, words []string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

// titleLikeNode reports whether a node is a headline container rather than
// publication metadata. VSB-style sites wrap both the title and the date inside
// one `.art-tit.cont-tit` block, so this is a hint, not a veto; an explicit
// 「发布日期」 marker still wins.
func titleLikeNode(n *html.Node) bool {
	key := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	return strings.Contains(key, "tit") || strings.Contains(key, "title") || strings.Contains(key, "headline")
}

// nonPublicationDate reports whether the matched date sits in a deadline/event
// context. A date at the very beginning of the text is treated as a list-style
// leading date (「2026/08/18 通知标题」) and kept. The look-back window is counted
// in runes: a byte window would cover fewer than five Chinese characters.
func nonPublicationDate(text, matched string) bool {
	if middle := strings.Index(text, matched); middle > 0 {
		prefix := []rune(text[:middle])
		if len(prefix) > 16 {
			prefix = prefix[len(prefix)-16:]
		}
		return containsAnyWord(string(prefix), nonPublishDateWords)
	}
	if strings.HasPrefix(text, matched) {
		return false
	}
	return containsAnyWord(text, nonPublishDateWords)
}

// publicationDate returns the first complete date in text that is not a
// deadline/event date. Text such as 「…（试用有效期：即日起--2026年12月31日）」
// only yields the publication date when nothing better is present.
func publicationDate(text string) (string, string) {
	haystack := text
	for {
		found := fullDateRE.FindString(haystack)
		if found == "" {
			return "", ""
		}
		if !nonPublicationDate(haystack, found) {
			parts := datePartsRE.FindAllString(found, -1)
			if len(parts) >= 3 {
				y, _ := strconv.Atoi(parts[0])
				m, _ := strconv.Atoi(parts[1])
				d, _ := strconv.Atoi(parts[2])
				if validDate(y, m, d) {
					return fmt.Sprintf("%04d-%02d-%02d", y, m, d), found
				}
			}
		}
		next := strings.Index(haystack, found) + len(found)
		if next >= len(haystack) {
			return "", ""
		}
		haystack = haystack[next:]
	}
}

// labelledDateIn reads a date from a node that marks it as metadata: a <time>
// element or a class/id containing date/time. Free text is not consulted here,
// so this is safe on containers that also hold other rows.
func labelledDateIn(n, articleLink *html.Node) (string, string) {
	var date, original string
	walk(n, func(v *html.Node) bool {
		if date != "" || (v == articleLink && v != n) {
			return false
		}
		if v.Type != html.ElementNode {
			return true
		}
		class := strings.ToLower(attr(v, "class") + " " + attr(v, "id"))
		if v.Data == "time" || strings.Contains(class, "date") || strings.Contains(class, "time") {
			s := attr(v, "datetime")
			if s == "" {
				s = textContent(v)
			}
			d, o := dateValue(s)
			if o != "" && !nonPublicationDate(s, o) {
				date, original = d, o
			}
		}
		return true
	})
	return date, original
}

// dateStampIn finds an element whose entire text is one complete date. Several
// templates render the publication date next to the headline in a bare
// <span>/<font> with no class hint (jxgc 首页, bs 首页, sqjsb 通知列表), and the
// anchor wraps both, so the pure-date element is the only reliable signal. The
// search stays inside this row: anchors of neighbouring rows are skipped.
func dateStampIn(n, articleLink *html.Node) (string, string) {
	var date, original string
	walk(n, func(v *html.Node) bool {
		if date != "" {
			return false
		}
		if v.Type != html.ElementNode || v.Data == "script" || v.Data == "style" {
			return true
		}
		if v.Data == "a" && v != articleLink {
			return false
		}
		if v == n || v.FirstChild == nil {
			return true
		}
		for child := v.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode {
				return true
			}
		}
		text := clean(textContent(v))
		if utf8.RuneCountInString(text) > 20 {
			return true
		}
		if d, o := dateValue(text); d != "" && o != "" && !nonPublicationDate(text, o) {
			date, original = d, text
		}
		return true
	})
	return date, original
}

func dateInNode(n, articleLink *html.Node) (string, string) {
	// Prefer explicitly marked dates; article titles may mention a different event date.
	if date, original := labelledDateIn(n, articleLink); original != "" {
		return date, original
	}
	if date, original := dateStampIn(n, articleLink); original != "" {
		return date, original
	}
	if n == articleLink {
		return "", ""
	}
	// A container with several anchors holds sibling rows (other articles, MORE
	// links, navigation). Its raw text therefore describes other items, and a date
	// inside it must not be attributed to this article.
	if countLinks(n) > 1 {
		return "", ""
	}
	var b strings.Builder
	walk(n, func(v *html.Node) bool {
		if v == articleLink && n != articleLink {
			return false
		}
		// A foreign anchor starts a sibling row (another article, a MORE link or a
		// navigation item). Aggregating its text is what made a neighbouring row's
		// summary date look like this row's publication date, so skip it. Sibling
		// text nodes and date spans outside foreign anchors are still considered.
		if v.Type == html.ElementNode && v.Data == "a" && v != articleLink {
			return false
		}
		if v.Type == html.ElementNode && (v.Data == "script" || v.Data == "style") {
			return false
		}
		if v.Type == html.TextNode {
			b.WriteString(v.Data)
			b.WriteByte(' ')
		}
		return true
	})
	return publicationDateOrPartial(clean(b.String()))
}

// publicationDateOrPartial prefers a complete publication date; when every
// complete date in the text is a deadline/event date it declines rather than
// inventing one, and only then falls back to a partial month/day.
func publicationDateOrPartial(text string) (string, string) {
	if date, original := publicationDate(text); original != "" {
		return date, original
	}
	if found := partialDateRE.FindStringSubmatch(text); len(found) > 1 {
		if !nonPublicationDate(text, found[1]) {
			return "", clean(found[1])
		}
	}
	return "", ""
}

func countLinks(n *html.Node) int {
	count := 0
	walk(n, func(v *html.Node) bool {
		if count > 3 {
			return false
		}
		if v.Type == html.ElementNode && v.Data == "a" {
			count++
		}
		return true
	})
	return count
}

func articleDate(a *html.Node) (string, string) {
	for n, depth := a, 0; n != nil && depth < 5; n, depth = n.Parent, depth+1 {
		// Stop before a container that holds other links: its text belongs to
		// sibling rows (other articles, MORE links, navigation) and the dates in
		// it are not this article's publication date.
		if n != a && (n.Data == "ul" || n.Data == "ol" || n.Data == "body" || countLinks(n) > 1) {
			break
		}
		if d, original := dateInNode(n, a); original != "" {
			return d, original
		}
		if n.Data == "li" || n.Data == "tr" {
			break
		}
	}
	return "", ""
}

// trimEdgeDate removes a leading/trailing date that duplicates the article's
// parsed publication date but uses a different notation (for example the list
// shows a split date while the title repeats it as ISO). Dates inside the title
// are left alone.
func trimEdgeDate(title, date string) string {
	if date == "" {
		return title
	}
	y, m, d := date[0:4], date[5:7], date[8:10]
	mi, _ := strconv.Atoi(m)
	di, _ := strconv.Atoi(d)
	variants := []string{
		date,
		fmt.Sprintf("%s/%s/%s", y, m, d),
		fmt.Sprintf("%s.%s.%s", y, m, d),
		fmt.Sprintf("%s年%d月%d日", y, mi, di),
		fmt.Sprintf("%s年%02d月%02d日", y, mi, di),
	}
	for _, variant := range variants {
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(title), variant), variant))
		if trimmed != strings.TrimSpace(title) && trimmed != "" {
			title = clean(trimmed)
		}
	}
	return title
}

// trailingDate extracts a publication date printed at the very end of a
// headline. Some templates put the date inside the article anchor with no
// markup of its own (jxgc 首页: 「…圆满完成2026级迎新工作<span>2026-09-15</span>」),
// so it reaches us as part of the title and no row-level date node exists.
func trailingDate(title string) (string, string) {
	trimmed := strings.TrimSpace(title)
	match := trailingDateRE.FindString(trimmed)
	if match == "" {
		return "", ""
	}
	parts := datePartsRE.FindAllString(match, -1)
	if len(parts) < 3 {
		return "", ""
	}
	y, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	d, _ := strconv.Atoi(parts[2])
	if !validDate(y, m, d) {
		return "", ""
	}
	// A headline that is nothing but a date is not an article title.
	if strings.TrimSpace(strings.TrimSuffix(trimmed, match)) == "" {
		return "", ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d), strings.TrimSpace(match)
}

func articleURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if articlePathRE.MatchString(u.Path) {
		return true
	}
	q := u.Query()
	for _, key := range []string{"newsid", "newsId", "infoid", "infoId", "articleid", "articleId", "contentid"} {
		if q.Get(key) != "" {
			return true
		}
	}
	return false
}

func usefulTitle(title string) bool {
	length := utf8.RuneCountInString(title)
	// Faculty profile titles and a few document names are only two or three
	// characters. URL/date checks, rather than headline length, distinguish them
	// from ordinary navigation links.
	if length < 2 || length > 350 {
		return false
	}
	for _, s := range []string{"点击查看", "查看详情", "阅读全文", "网站地图", "版权所有", "上一页", "下一页", "访问量", "更多"} {
		if strings.HasPrefix(title, s) && length < 15 {
			return false
		}
	}
	return !pageNumberRE.MatchString(title)
}

// snippet keeps the searchable text bounded while retaining the public summary
// commonly rendered beside an article link. It is metadata, not a copy of the
// article body.
func snippet(s string, limit int) string {
	s = clean(s)
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit])
	}
	return s
}

func articleSnippet(a *html.Node, title, dateText string) string {
	if a == nil {
		return ""
	}
	text := textContent(a)
	text = strings.TrimSpace(strings.TrimPrefix(text, title))
	if dateText != "" {
		text = strings.ReplaceAll(text, dateText, " ")
	}
	return snippet(text, 1200)
}

func detailContent(doc *html.Node) string {
	var best string
	walk(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data == "script" || n.Data == "style" || n.Data == "noscript" {
			return n.Type != html.ElementNode
		}
		key := strings.ToLower(attr(n, "id") + " " + attr(n, "class"))
		if n.Data == "article" || strings.Contains(key, "vsb_content") || strings.Contains(key, "v_news_content") || strings.Contains(key, "article-body") || strings.Contains(key, "art-body") {
			candidate := textContent(n)
			if len([]rune(candidate)) > len([]rune(best)) {
				best = candidate
			}
			return false
		}
		return true
	})
	return snippet(best, 4000)
}

// excludedTree reports document chrome that never holds article or column data:
// page footers, friend links, copyright lines and breadcrumbs. script/style
// payloads are dropped as well because scripts render dates embedded in JS.
func excludedTree(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if n.Data == "footer" || n.Data == "script" || n.Data == "style" || n.Data == "noscript" {
		return true
	}
	class := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	for _, word := range []string{"footer", "friendlink", "copyright", "breadcrumb", "crumb", "position", "nexthtml"} {
		if strings.Contains(class, word) {
			return true
		}
	}
	return false
}

// excludedArticleTree is excludedTree plus navigation UI. Article headlines never
// live in a menu or sidebar, so the detail-page title search skips them: a menu
// link or a header <h1> repeating the site name must not shadow the headline.
func excludedArticleTree(n *html.Node) bool {
	if excludedTree(n) {
		return true
	}
	if n.Type != html.ElementNode {
		return false
	}
	for _, key := range []string{attr(n, "class"), attr(n, "id")} {
		for _, token := range strings.Fields(strings.ToLower(key)) {
			if navClassWord(token) {
				return true
			}
		}
	}
	return false
}

// navClassWord reports whether one CSS class/id token names a navigation block.
// Matching at token boundaries keeps 「right_nav」/「nav-b」 in and unrelated words
// such as 「navbtn」 or 「candidate」 out.
func navClassWord(token string) bool {
	for _, word := range []string{"nav", "menu", "sidebar", "side-bar", "foot", "sitemap", "breadcrumb", "crumb"} {
		if token == word || strings.HasPrefix(token, word+"-") || strings.HasSuffix(token, "-"+word) ||
			strings.HasPrefix(token, word+"_") || strings.HasSuffix(token, "_"+word) {
			return true
		}
	}
	return false
}

// insideNavigationChrome reports whether a node sits in a menu, sidebar or footer
// block. Section entry pages (dfl.cdu.edu.cn/yjsjy.htm, xzbgs.cdu.edu.cn/xzzq1.htm)
// used to be collected as articles from exactly those blocks; real documents are
// also linked there (jw.cdu.edu.cn/info/1191/12080.htm), so the subtree is not
// pruned and only non-article URLs are rejected.
func insideNavigationChrome(n *html.Node) bool {
	for p := n; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if p.Data == "body" || p.Data == "html" {
			return false
		}
		if p.Data == "nav" {
			return true
		}
		for _, key := range []string{attr(p, "class"), attr(p, "id")} {
			for _, token := range strings.Fields(strings.ToLower(key)) {
				if navClassWord(token) {
					return true
				}
			}
		}
	}
	return false
}

// navigationLabels are link texts that name a place rather than a document.
// They are never article headlines, even when the surrounding row aggregates a
// date from a header, breadcrumb or pagination block.
var navigationLabels = []string{
	"首页", "网站首页", "返回首页", "首页动态", "更多", "more", "上一页", "下一页",
	"尾页", "返回", "返回列表", "当前位置", "您现在的位置", "网站地图", "联系我们",
	"版权所有", "上一条", "下一条", "没有了", "登录", "注册", "打印本页", "关闭窗口",
}

// navigationPrefixes are labels that are followed by page context, for example
// 「下一条：…」 in the detail footer or 「当前位置：首页 > …」 in a breadcrumb.
var navigationPrefixes = []string{"当前位置", "您现在的位置", "上一条", "下一条", "更多", "网站地图", "版权所有", "返回首页", "返回列表", "返回顶部"}

func navigationTitle(title string) bool {
	name := strings.ToLower(compactChinese(title))
	if name == "" {
		return false
	}
	for _, label := range navigationLabels {
		if name == label {
			return true
		}
	}
	for _, prefix := range navigationPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// navigationPageURL reports URLs that are section entry pages rather than
// documents: index/default pages, site maps and the common 「下载专区/办事指南/
// 联系我们」 column pages. The list-page review recorded 15 such pages that were
// collected as articles because a nearby row happened to show a date.
var (
	navigationPageNames  = []string{"index", "default", "main", "map", "sitemap", "search", "more", "wzdt", "ztdh"}
	navigationPageTokens = []string{"xzzq", "bszn", "lxwm", "sitemap", "wzdt", "ztdh", "ztzl"}
)

func navigationPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	p := strings.ToLower(u.Path)
	if p == "" || p == "/" {
		return true
	}
	base := strings.TrimSuffix(path.Base(p), path.Ext(p))
	for _, name := range navigationPageNames {
		if base == name {
			return true
		}
	}
	for _, token := range navigationPageTokens {
		if strings.Contains(base, token) {
			return true
		}
	}
	return false
}

func linkTitle(n *html.Node) string {
	title := clean(attr(n, "title"))
	if title != "" {
		return title
	}
	// Semantic headings take precedence over title wrappers, since a wrapper
	// can also contain a date and a multi-paragraph summary.
	walk(n, func(v *html.Node) bool {
		if title != "" {
			return false
		}
		if v == n {
			return true
		}
		if v.Type != html.ElementNode {
			return true
		}
		if v.Data == "h1" || v.Data == "h2" || v.Data == "h3" || v.Data == "h4" || v.Data == "h5" || v.Data == "h6" {
			if candidate := textContent(v); usefulTitle(candidate) {
				title = candidate
				return false
			}
		}
		return true
	})
	if title == "" {
		walk(n, func(v *html.Node) bool {
			if title != "" {
				return false
			}
			if v == n || v.Type != html.ElementNode {
				return true
			}
			for _, class := range strings.Fields(strings.ToLower(attr(v, "class"))) {
				if strings.Contains(class, "title") || strings.Contains(class, "tit") || class == "tt" || strings.HasSuffix(class, "-tt") || strings.HasSuffix(class, "_tt") {
					if candidate := textContent(v); usefulTitle(candidate) {
						title = candidate
						return false
					}
				}
			}
			return true
		})
	}
	if title == "" {
		var b strings.Builder
		walk(n, func(v *html.Node) bool {
			if v.Type == html.ElementNode {
				if v.Data == "script" || v.Data == "style" {
					return false
				}
				if v != n {
					key := strings.ToLower(attr(v, "class"))
					for _, word := range []string{"desc", "summary", "excerpt", "brief", "intro", "txt-right-p", "date", "time", "more"} {
						if strings.Contains(key, word) {
							return false
						}
					}
				}
			}
			if v.Type == html.TextNode {
				b.WriteString(v.Data)
				b.WriteByte(' ')
			}
			return true
		})
		title = clean(b.String())
	}
	if title == "" && n.Parent != nil && countLinks(n.Parent) == 1 {
		walk(n.Parent, func(v *html.Node) bool {
			if title != "" || v == n {
				return false
			}
			if v.Type == html.ElementNode && (v.Data == "h2" || v.Data == "h3" || v.Data == "h4") {
				title = textContent(v)
				return false
			}
			return true
		})
	}
	return title
}

var channelWords = []string{"通知", "公告", "通告", "新闻", "动态", "快讯", "资讯", "要闻", "党务", "校务", "业务", "流程", "规章", "制度", "政策", "文件", "下载", "表格", "公示", "招标", "采购", "招聘", "招生", "就业", "教学", "科研", "学术", "党建", "工会", "团学", "学生工作", "人才", "培训", "交流", "管理办法", "学习", "廉政", "理论", "服务指南", "办事指南", "党群", "工作简报", "成果", "学科", "讲座", "活动", "国际合作"}

func channelName(name string) bool {
	length := utf8.RuneCountInString(name)
	if length < 2 || length > 24 {
		return false
	}
	for _, word := range []string{"登录", "登陆", "邮箱", "入口", "OA", "平台", "系统", "新闻中心网站", "网站首页", "领导信箱"} {
		if strings.Contains(name, word) {
			return false
		}
	}
	for _, word := range channelWords {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func compactChinese(s string) string {
	runes := []rune(clean(s))
	var b strings.Builder
	for i, r := range runes {
		if r == ' ' && i > 0 && i+1 < len(runes) && unicode.Is(unicode.Han, runes[i-1]) && unicode.Is(unicode.Han, runes[i+1]) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func headingText(n *html.Node) string {
	var b strings.Builder
	walk(n, func(v *html.Node) bool {
		if v.Type == html.ElementNode && (v.Data == "script" || v.Data == "style" || v.Data == "a" || hasClass(v, "more")) {
			return false
		}
		if v.Type == html.TextNode {
			b.WriteString(v.Data)
			b.WriteByte(' ')
		}
		return true
	})
	return compactChinese(b.String())
}

func nearbyHeading(a *html.Node) string {
	for n, depth := a.Parent, 0; n != nil && depth < 8 && n.Data != "body"; n, depth = n.Parent, depth+1 {
		var found string
		walk(n, func(v *html.Node) bool {
			if found != "" || v == a {
				return false
			}
			if v.Type != html.ElementNode {
				return true
			}
			if hasClass(v, "nav-tabs") {
				walk(v, func(tab *html.Node) bool {
					if found != "" {
						return false
					}
					if tab.Type == html.ElementNode && tab.Data == "a" {
						candidate := compactChinese(textContent(tab))
						if channelName(candidate) {
							found = candidate
						}
						return false
					}
					return true
				})
				return false
			}
			if v.Type == html.ElementNode && (v.Data == "ul" || v.Data == "li" || v.Data == "script" || v.Data == "style") {
				return false
			}
			if v.Data == "a" {
				return false
			}
			key := strings.ToLower(attr(v, "class") + " " + attr(v, "id"))
			if v.Data == "h1" || v.Data == "h2" || v.Data == "h3" || v.Data == "h4" || v.Data == "span" || v.Data == "strong" || strings.Contains(key, "tit") {
				name := headingText(v)
				if channelName(name) {
					found = name
					return false
				}
			}
			return true
		})
		if found != "" {
			return found
		}
	}
	return ""
}

func parseChannels(doc *html.Node, base, sourceID string) []model.Channel {
	var channels []model.Channel
	seen := map[string]int{}
	priority := func(s string) int {
		if strings.Contains(s, "通知") || strings.Contains(s, "公告") || strings.Contains(s, "通告") {
			return 0
		}
		if strings.Contains(s, "新闻") || strings.Contains(s, "动态") {
			return 1
		}
		return 2
	}
	walk(doc, func(a *html.Node) bool {
		if excludedTree(a) {
			return false
		}
		if a.Type != html.ElementNode || a.Data != "a" {
			return true
		}
		raw := resolveURL(base, attr(a, "href"))
		if raw == "" || !sameSite(raw, base) || protectedURL(raw) || articleURL(raw) || raw == base {
			return false
		}
		u, _ := url.Parse(raw)
		ext := strings.ToLower(path.Ext(u.Path))
		if ext != "" && ext != ".htm" && ext != ".html" && ext != ".shtml" && ext != ".php" && ext != ".jsp" && ext != ".aspx" {
			return false
		}
		name := compactChinese(linkTitle(a))
		if name == "" || strings.EqualFold(name, "more") || strings.Contains(name, "更多") || name == ">>" || name == "+" {
			name = nearbyHeading(a)
		}
		if !channelName(name) {
			return false
		}
		if index, exists := seen[raw]; exists {
			if priority(name) < priority(channels[index].Name) {
				channels[index].Name = name
			}
			return false
		}
		// The link text decides whether this is a column entry. A column header
		// such as 「通知公告」 often shares its <li> with a 「最新日期」 badge
		// (`<li><a href="tzgg.htm">通知公告</a><span>09-14</span></li>`), and the
		// old row-date test dropped those columns. Article-like links never reach
		// here, so the stray date must not veto a column link.
		seen[raw] = len(channels)
		channels = append(channels, model.Channel{ID: stableID(sourceID, raw), Name: name, URL: raw})
		return false
	})
	// Notices first so a configured channel budget always covers the highest-value columns.
	sort.SliceStable(channels, func(i, j int) bool {
		return priority(channels[i].Name) < priority(channels[j].Name)
	})
	return channels
}

func parseArticles(doc *html.Node, base string, source model.Source, channel string, fetchedAt string) []model.Article {
	var articles []model.Article
	seen := map[string]bool{}
	walk(doc, func(a *html.Node) bool {
		if excludedTree(a) {
			return false
		}
		if a.Type != html.ElementNode || a.Data != "a" {
			return true
		}
		raw := resolveURL(base, attr(a, "href"))
		if raw == "" || protectedURL(raw) || seen[raw] {
			return false
		}
		title := linkTitle(a)
		if !usefulTitle(title) {
			return false
		}
		date, original := articleDate(a)
		// Some list templates put the date inside the anchor's own text, so it
		// arrives as part of the headline. Recover it before deciding whether
		// this anchor is an article at all.
		if original == "" {
			if d, o := trailingDate(title); o != "" {
				date, original = d, o
				title = clean(strings.TrimSuffix(strings.TrimSpace(title), o))
			}
		}
		if !sameSite(raw, base) && (!articleURL(raw) || original == "") {
			return false
		}
		if !articleURL(raw) && original == "" {
			return false
		}
		// A whole-row anchor can include its date; remove only edge dates, not dates within a title.
		if original != "" {
			title = clean(strings.TrimSuffix(strings.TrimPrefix(title, original), original))
		}
		// Some templates repeat the publication date in another notation than the
		// one that was matched (split-block date plus an ISO copy). Drop such an
		// edge duplicate only when it equals the parsed date, so headlines that
		// merely mention a date stay intact.
		title = trimEdgeDate(title, date)
		if !usefulTitle(title) {
			return false
		}
		// A URL without an article path only qualifies when the link itself names
		// a document. Row aggregation used to accept breadcrumbs (「首页」),
		// section entry pages (index.htm, xzzq1.htm, bszn.htm) and pagination
		// labels as soon as any date appeared nearby; navigation blocks are where
		// such links live.
		if !articleURL(raw) && (navigationTitle(title) || navigationPageURL(raw) || insideNavigationChrome(a)) {
			return false
		}
		name := channel
		if name == "首页动态" {
			if heading := nearbyHeading(a); heading != "" {
				name = heading
			}
		}
		articles = append(articles, model.Article{ID: stableID(source.ID, raw), Title: title, Content: articleSnippet(a, title, original), URL: raw, SourceID: source.ID, SourceName: source.Name, SourceKind: source.Kind, Channel: name, Date: date, DateText: original, FetchedAt: fetchedAt})
		seen[raw] = true
		return false
	})
	return articles
}

func nextPage(doc *html.Node, base string) string {
	var next string
	walk(doc, func(a *html.Node) bool {
		if next != "" {
			return false
		}
		if a.Type != html.ElementNode || a.Data != "a" {
			return true
		}
		label := strings.ToLower(clean(textContent(a)))
		if strings.Contains(label, "下一页") || strings.Contains(label, "下页") || label == "next" || strings.EqualFold(attr(a, "rel"), "next") {
			raw := resolveURL(base, attr(a, "href"))
			if raw != "" && raw != base && sameSite(raw, base) && !articleURL(raw) {
				next = raw
			}
		}
		return false
	})
	return next
}

// detailInfo is the metadata extracted from an article detail page.
//
// DateExplicit reports whether the value came from a publication-date marker
// (meta PubDate/datePublished, an explicit 「发布时间/发布日期/发表时间」 label, or
// the 「时间：… 浏览」 metadata line) instead of a headline or a body container
// that merely happens to contain a date. Only an explicit marker may replace a
// date that the list page already produced.
type detailInfo struct {
	Title        string
	Date         string
	DateText     string
	DateExplicit bool
}

// detailMetadata keeps the legacy three-value contract for callers and tests.
func detailMetadata(doc *html.Node) (title, date, original string) {
	info := detailMetadataInfo(doc)
	return info.Title, info.Date, info.DateText
}

// siteNameFromTitle reads the site name from the document <title>, which VSB and
// most cdu templates append after the article headline: 「标题-成都大学期刊中心」.
// A <title> that is nothing but a unit name (「成都大学信息网络中心」) already is
// the site name. It is used to reject a page-header <h1> that merely repeats it.
func siteNameFromTitle(doc *html.Node) string {
	var raw string
	walk(doc, func(n *html.Node) bool {
		if raw != "" {
			return false
		}
		if n.Type == html.ElementNode && n.Data == "title" {
			raw = clean(textContent(n))
			return false
		}
		return true
	})
	if raw == "" {
		return ""
	}
	for _, separator := range []string{" - ", " — ", " – ", " | ", " _ ", "－", "-", "—", "–", "|", "_"} {
		if index := strings.LastIndex(raw, separator); index > 0 {
			name := compactChinese(raw[index+len(separator):])
			if utf8.RuneCountInString(name) >= 2 {
				return name
			}
		}
	}
	// No 「-站点名」 suffix: a short <title> that ends like an organisational unit
	// is the site name itself.
	name := compactChinese(raw)
	if utf8.RuneCountInString(name) <= 30 {
		for _, suffix := range []string{"大学", "学院", "中心", "办公室", "工作部", "宣传部", "图书馆", "委员会", "研究所", "研究院"} {
			if strings.HasSuffix(name, suffix) {
				return name
			}
		}
	}
	return ""
}

// detailTitleCandidates selects the detail headline. A VSB detail page renders
// the real headline as h2-h4 inside `.art-tit/.cont-tit/.news-title`, while an
// <h1> in the page header often repeats the site name. The old code accepted the
// first h1 it met, so 成都大学信息网络中心 shadowed 关于校园网维护的通知.
func detailTitleCandidates(doc *html.Node, siteName string) string {
	bestRank := 99
	var best string
	walk(doc, func(n *html.Node) bool {
		if excludedArticleTree(n) {
			return false
		}
		if n.Type != html.ElementNode {
			return true
		}
		rank := 99
		switch {
		case n.Data == "h2" || n.Data == "h3" || n.Data == "h4" || n.Data == "h5":
			for parent, depth := n.Parent, 0; parent != nil && depth < 3; parent, depth = parent.Parent, depth+1 {
				key := strings.ToLower(attr(parent, "class") + " " + attr(parent, "id"))
				if strings.Contains(key, "art-tit") || strings.Contains(key, "cont-tit") || strings.Contains(key, "news-title") || strings.Contains(key, "article-title") || strings.Contains(key, "art_title") || strings.Contains(key, "article_title") {
					rank = 1
					break
				}
			}
		case hasClass(n, "arti_title") || hasClass(n, "article-title") || hasClass(n, "art-title"):
			rank = 2
		case n.Data == "h1":
			rank = 3
		}
		if rank == 99 || rank >= bestRank {
			return true
		}
		candidateText := textContent(n)
		if !usefulTitle(candidateText) || navigationTitle(candidateText) {
			return true
		}
		if siteName != "" && rank > 1 && compactChinese(candidateText) == siteName {
			// The page header repeats the site name; the real headline is elsewhere.
			return true
		}
		best, bestRank = candidateText, rank
		return true
	})
	return best
}

// insideArticleBody reports whether a node sits inside the article body, where a
// 「时间：…」 line describes an event rather than the publication of the page.
func insideArticleBody(n *html.Node) bool {
	for parent, depth := n, 0; parent != nil && depth < 6; parent, depth = parent.Parent, depth+1 {
		if parent.Type != html.ElementNode {
			continue
		}
		if parent.Data == "article" {
			return true
		}
		key := strings.ToLower(attr(parent, "class") + " " + attr(parent, "id"))
		if strings.Contains(key, "vsb_content") || strings.Contains(key, "v_news_content") || strings.Contains(key, "article-body") || strings.Contains(key, "art-body") {
			return true
		}
	}
	return false
}

// publicationMetaLine reports whether text is the compact 「时间：2015-12-02
// 浏览量：120」 metadata line that VSB templates print under the headline.
func publicationMetaLine(text string) bool {
	if !strings.Contains(text, "时间：") && !strings.Contains(text, "时间:") {
		return false
	}
	if !strings.Contains(text, "浏览") && !strings.Contains(text, "点击") {
		return false
	}
	return utf8.RuneCountInString(text) <= 80
}

// explicitPublicationNode reports whether a node is real publication metadata
// rather than a container that merely happens to hold a date. Only such a node
// may replace a date that a list page already reported.
func explicitPublicationNode(n *html.Node, text string) bool {
	key := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	if containsAnyWord(text, publishDateWords) || containsAnyWord(key, publishDateWords) {
		return true
	}
	if !publicationMetaLine(text) || insideArticleBody(n) {
		return false
	}
	// 「时间：… 浏览量：…」 is metadata when it carries a metadata class or sits
	// inside the headline block, as VSB does with `.art-tit .rq`.
	for _, word := range []string{"date", "time", "author", "rq", "info", "source"} {
		if strings.Contains(key, word) {
			return true
		}
	}
	for parent, depth := n.Parent, 0; parent != nil && depth < 3; parent, depth = parent.Parent, depth+1 {
		if titleLikeNode(parent) {
			return true
		}
	}
	return false
}

func detailMetadataInfo(doc *html.Node) detailInfo {
	var info detailInfo
	siteName := siteNameFromTitle(doc)
	walk(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data != "meta" {
			return true
		}
		key := strings.ToLower(attr(n, "name") + " " + attr(n, "property"))
		value := clean(attr(n, "content"))
		if strings.Contains(key, "articletitle") || strings.Contains(key, "og:title") {
			if usefulTitle(value) && !navigationTitle(value) {
				info.Title = value
			}
		}
		if strings.Contains(key, "pubdate") || strings.Contains(key, "published") || strings.Contains(key, "publishdate") || strings.Contains(key, "datepublished") || strings.Contains(key, "dcterms.created") {
			if d, o := dateValue(value); d != "" {
				info.Date, info.DateText, info.DateExplicit = d, o, true
			}
		}
		return true
	})
	if info.Title == "" {
		info.Title = detailTitleCandidates(doc, siteName)
	}
	if info.Date != "" {
		return info
	}
	// Fallback for pages without a machine-readable date. Every element is scored
	//   0. it contains an explicit 「发布时间/发布日期」 marker;
	//   1. its class/id looks like publication metadata (date|time|info) or it is
	//      the common 「时间：… 浏览」 line;
	//   2. it is a headline container (VSB wraps title and date in `.art-tit`).
	// The lowest rank wins and, inside a rank, the shortest text: the tightest
	// node around the date is the one that actually describes this article. Text
	// mentioning 「有效期/截止/将于…」 is skipped by publicationDate, so a title like
	// 「…（试用有效期：即日起--2026年12月31日）」 can no longer overwrite the real
	// 发布时间 printed below it.
	type candidate struct {
		rank     int
		size     int
		date     string
		original string
		node     *html.Node
		text     string
	}
	best := candidate{rank: -1}
	walk(doc, func(n *html.Node) bool {
		if excludedTree(n) {
			return false
		}
		if n.Type != html.ElementNode {
			return true
		}
		key := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
		t := textContent(n)
		explicit := containsAnyWord(t, publishDateWords) || containsAnyWord(key, publishDateWords)
		labelled := strings.Contains(key, "date") || strings.Contains(key, "time") || strings.Contains(key, "info") ||
			((strings.Contains(t, "时间：") || strings.Contains(t, "时间:")) && strings.Contains(t, "浏览"))
		rank := -1
		switch {
		case explicit:
			rank = 0
		case labelled && !titleLikeNode(n):
			rank = 1
		case labelled && titleLikeNode(n):
			rank = 2
		}
		if rank < 0 {
			return true
		}
		if best.rank >= 0 && rank > best.rank {
			return true
		}
		size := len([]rune(t))
		if best.rank == rank && size >= best.size {
			return true
		}
		if d, o := publicationDate(t); o != "" {
			best = candidate{rank: rank, size: size, date: d, original: o, node: n, text: t}
		}
		return true
	})
	info.Date, info.DateText = best.date, best.original
	// Only a 「发布时间/发布日期」 marker (or the equivalent meta tag above) is
	// strong enough evidence to replace a date the list page already reported.
	// A headline or body block that merely contains a date keeps its weaker
	// status, so it only fills in a missing list date.
	if best.node != nil {
		info.DateExplicit = best.rank == 0 || explicitPublicationNode(best.node, best.text)
	}
	return info
}
