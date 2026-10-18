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

func dateInNode(n, articleLink *html.Node) (string, string) {
	var date, original string
	// Prefer explicitly marked dates; article titles may mention a different event date.
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
			if o != "" {
				date, original = d, o
			}
		}
		return true
	})
	if original != "" {
		return date, original
	}
	if n == articleLink {
		return "", ""
	}
	var b strings.Builder
	walk(n, func(v *html.Node) bool {
		if v == articleLink && n != articleLink {
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
	return dateValue(clean(b.String()))
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
		if n != a && (n.Data == "ul" || n.Data == "ol" || n.Data == "body" || countLinks(n) > 3) {
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

func excludedTree(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if n.Data == "footer" || n.Data == "script" || n.Data == "style" || n.Data == "noscript" {
		return true
	}
	class := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	return strings.Contains(class, "footer") || strings.Contains(class, "friendlink")
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
		// Inspect only the link's immediate row here. A MORE/header link shares a
		// section with dated articles, which must not turn that header into an article.
		if a.Parent != nil && (a.Parent.Data == "li" || a.Parent.Data == "tr" || countLinks(a.Parent) == 1) {
			if _, date := dateInNode(a.Parent, a); date != "" {
				return false
			}
		}
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
		if !usefulTitle(title) {
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

func detailMetadata(doc *html.Node) (title, date, original string) {
	walk(doc, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return true
		}
		if n.Data == "meta" {
			key := strings.ToLower(attr(n, "name") + " " + attr(n, "property"))
			value := clean(attr(n, "content"))
			if strings.Contains(key, "articletitle") || strings.Contains(key, "og:title") {
				if usefulTitle(value) {
					title = value
				}
			}
			if strings.Contains(key, "pubdate") || strings.Contains(key, "published") || strings.Contains(key, "publishdate") || strings.Contains(key, "datepublished") {
				if d, o := dateValue(value); d != "" {
					date, original = d, o
				}
			}
		}
		inArticleTitle := false
		if n.Data == "h2" || n.Data == "h3" || n.Data == "h4" {
			for parent, depth := n.Parent, 0; parent != nil && depth < 3; parent, depth = parent.Parent, depth+1 {
				key := strings.ToLower(attr(parent, "class") + " " + attr(parent, "id"))
				if strings.Contains(key, "art-tit") || strings.Contains(key, "cont-tit") || strings.Contains(key, "news-title") || strings.Contains(key, "article-title") {
					inArticleTitle = true
					break
				}
			}
		}
		if title == "" && (n.Data == "h1" || hasClass(n, "arti_title") || inArticleTitle) {
			if candidate := textContent(n); usefulTitle(candidate) {
				title = candidate
			}
		}
		return true
	})
	if date != "" {
		return
	}
	// Limit fallback to publication metadata; body event dates and footer copyright years are not publication dates.
	walk(doc, func(n *html.Node) bool {
		if date != "" || excludedTree(n) {
			return false
		}
		if n.Type != html.ElementNode {
			return true
		}
		key := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
		t := textContent(n)
		if len(t) > 1000 {
			return true
		}
		if strings.Contains(key, "date") || strings.Contains(key, "time") || strings.Contains(key, "info") || strings.Contains(t, "发布时间") || strings.Contains(t, "发布日期") || strings.Contains(t, "发表时间") || ((strings.Contains(t, "时间：") || strings.Contains(t, "时间:")) && strings.Contains(t, "浏览")) {
			if d, o := dateValue(t); d != "" {
				date, original = d, o
				return false
			}
		}
		return true
	})
	return
}
