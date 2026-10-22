# crawler 包审查报告（日期识别链路 + 抓取健壮性）

- 审查对象：`internal/crawler/parser.go`、`internal/crawler/crawler.go`（只读，未修改任何 `.go`）
- 审查方式：代码逐行 + 真实站点抓取（`curl.exe` / Go `net/http`）+ `data/snapshot.json` 全量统计 + 把包内逻辑逐字拷贝到 `research/tmpreview-crawler/audit/` 后跑最小复现
- 副本保真性：`go run ./research/tmpreview-crawler/verify` 输出
  `parser.go orig(normalized)=3ea85e91ce1f7c89 copy=3ea85e91ce1f7c89 identical=true`；
  `crawler.go orig(normalized)=a36c6df5831bd25a copy=a36c6df5831bd25a identical=true`
  → 下文所有「实际输出」都来自真实解析逻辑，不是重写的近似实现。
- 抓取缓存：`research/tmpreview-crawler/cache/*.html`（2026-10-21 抓取，含 lib / xczgb / journal 三站列表页与详情页）
- 复现程序（全部可重跑）：
  `research/tmpreview-crawler/{cases,cases2,cases3,cases4,cases5,probe,trace,chain,rowscan,scan,charsetprobe,perf}/`
  对应输出已存为同名 `.out` 文件。

---

## 0. 结论速览

| # | 问题 | 严重度 | 根因位置 | 真实证据 |
|---|---|---|---|---|
| 1 | 详情页把**标题里的「试用有效期」**当发布时间，覆盖真实 `发布时间` | **高** | `parser.go:727-746`（`strings.Contains(key,"info")` / `strings.Contains(t,"发布时间")` 命中错误粒度节点，且文档序在前） | `lib/info/1057/1571.htm` → `2026-12-31`（应 `2022-05-16`）；`1540.htm` → `2026-12-31`（应 `2022-03-01`）；`1543.htm` → `2026-03-01`（应 `2022-03-08`） |
| 2 | 列表页 `articleDate` 向上聚合时**跳过自己这一行、把兄弟行的摘要日期**当本行发布时间 | **高** | `parser.go:281-294` + `dateInNode:224-246`（`v == articleLink` 直接跳过） | `journal.cdu.edu.cn` 首页三条政策法规行 → 快照 `1998-04-01 / 2005-12-01 / 2005-12-01`，与实际行文完全一致 |
| 3 | 列表行内日期节点缺失时，**行内摘要里的活动日期**被当发布时间 | 中 | `parser.go:250-264`（聚合整行文本后 `dateValue` 取第一个完整日期） | 最小复现 `cases.go` C 段：`2026-11-13`（活动日期） |
| 4 | 详情补全预算只挑 `Date==""` 的条目，**非空的错误日期永远得不到纠正** | 高 | `crawler.go:265-277` + `199-223` | `1571`/`1540`/`1332` 三者 Date 均非空，详情页正确值 `2022-05-16` / `2022-03-01` / `2015-12-02` 从未生效 |
| 5 | 标题里残留日期（只裁剪了 `DateText` 那一种格式） | 中 | `parser.go:646-649` | 快照 34 条，全部来自机械工程学院，如 `成都大学科学技术项目协作外拨事项公示 2026-09-14` |
| 6 | 非文章 URL（`index.htm`、面包屑「首页」）因所在行能聚合出日期被当文章收录 | 中 | `parser.go:640-645` + `excludedTree:372-381` | 快照 86 条非 `/info/` URL，其中 15 条是 `index.htm`/`xzzq1.htm`/`bszn.htm` 之类导航页 |
| 7 | `parseChannels` 把「同行带日期」的栏目链接当文章丢弃 | 低-中 | `parser.go:603-609` | `cases.go` E 段：`通知公告`+`<span>09-14</span>` → 栏目消失 |
| 8 | 详情标题识别会拿站点名/面包屑里的 `<h1>` | 低 | `parser.go:716` | `cases.go` D 段：`<h1>成都大学信息网络中心</h1>` 被当标题 |
| 9 | charset 处理依赖响应头与 meta 的 1024 字节窗口，出错时静默产生乱码 | 低（当前 cdu 站点均为 UTF-8 且无 CT charset，未触发） | `crawler.go:123` | `charsetprobe.out` [2][3] 两种情形均乱码 |

**不属于解析 bug 的源站数据问题**见第 6 节（`xczgb` 的 1922/1925/1927/1938、`dfl` 的 2002-03-03、`journal` 正文里的 2005/2015 等），已逐条与页面原文对照。

---

## 1. 日期识别链路逐函数说明

### 1.1 四类正则（`parser.go:20-28`）

| 变量 | 行 | 模式 | 匹配范围与边界 |
|---|---|---|---|
| `fullDateRE` | `parser.go:21` | `(?:19\|20)\d{2}\s*[-/.年]\s*\d{1,2}\s*[-/.月]\s*\d{1,2}日?` | 年 + 任意分隔符（`-` `/` `.` `年`）+ 月 + 分隔符（`-` `/` `.` `月`）+ 日。**不锚定边界**，可在任意长串中命中；分隔符可混用；不校验日历合法性（校验在 `validDate`） |
| `datePartsRE` | `parser.go:22` | `\d+` | 只用来拆 `fullDateRE` 命中的三段数字 |
| `partialDateRE` | `parser.go:23` | `(?:^\|[^\d])((?:0?[1-9]\|1[0-2])\s*[-/.月]\s*(?:0?[1-9]\|[12]\d\|3[01])日?)(?:[^\d]\|$)` | 只有月日、无年份。月 1-12、日 1-31 做数值约束，但**不校验组合合法性**（`02-30` 能中） |
| `splitDateRE` | `parser.go:24` | `(?:^\|\s)(\d{1,2})\s+((?:19\|20)\d{2})\s*[-/.年]\s*(\d{1,2})(?:月\|\s\|$)` | 「日 年-月」分块格式（VSB 日历块渲染成 `14` `2026-09` 两段，`textContent` 拼成 `14 2026-09`）。要求 `日` 前是行首或空白、`月` 后是「月」/空白/行尾 |

### 1.2 `dateValue`（`parser.go:190-214`）

优先级：`fullDateRE` → `splitDateRE` → `partialDateRE`，返回 `(ISO日期, 原文片段)`。

- `fullDateRE` 命中后取 `\d+` 三段按 年/月/日 解释，经 `validDate`（`parser.go:216-219`，用 `time.Date` 回读校验）才返回；**非法则继续往下走**。
- `splitDateRE` 命中后按 日/年/月 解释，同样过 `validDate`；返回的原文是 `clean(parts[0])`，即带前导空白的整段。
- `partialDateRE` 只返回原文、`date` 恒为 `""`——**这是「只给月日时绝不臆造年份」的设计**（`parser_test.go:77-79` 也在锁这个行为）。

实测边界（`cases.out` A 段 / `cases2.out` H 段）：

```
"发布时间：2022-05-16"        -> date="2022-05-16"  text="2022-05-16"
"试用有效期：即日起--2026年12月31日" -> date="2026-12-31"  text="2026年12月31日"   ← bug 1 的原料
"2026.08.12"                -> date="2026-08-12"  text="2026.08.12"
"2026/8/12"                 -> date="2026-08-12"  text="2026/8/12"
"09-14"                     -> date=""            text="09-14"
"12 2026.08"                -> date="2026-08-12"  text="12 2026.08"
"29 2019-11"                -> date="2019-11-29"  text="29 2019-11"
"2026年9月14日——2026年9月16日" -> date="2026-09-14"  text="2026年9月14日"   ← 取第一个，不看语境
"编号2026-1-23-45"           -> date="2026-01-23"  text="2026-1-23"        ← 编号被当日期
"2026-13-45"                -> date=""            text=""
"更新于 2026-02-30"          -> date=""            text="02-30"            ← 非法日期回落 partial，text 却是「02-30」
"共118条 第7页"               -> date=""            text=""
```

风险点：

1. **无边界锚定**：`编号2026-1-23-45` 这种编号、`2026-2027学年` 之外的连写数字都可能被切成日期。快照里 `2026-2027学年` 类标题未被误判（因为 `-2027` 后必须再有 `-月-日` 才成 `fullDateRE`），但规则本身靠「恰好凑够三段」兜底。
2. **非法日期的 `text` 具有误导性**：`2026-02-30` 返回 `("", "02-30")`，调用方拿到非空 `original` 就认为「这一行有日期」，`parseArticles:643-645` 的跨站放行、`crawler.go:203` 的合并判断都会把它当真。
3. `partialDateRE` 的 `(?:^|[^\d])` 前置断言在 `clean()` 之后、多段文本拼接处容易跨词命中（如 `时间：2026-11-13 浏览：120` 里的 `11-13`——不过该串先被 `fullDateRE` 吃掉，见 1.4 的 P2 用例）。

### 1.3 `dateInNode`（`parser.go:221-265`）

两阶段：

1. **显式类名优先**（`parser.go:224-243`）：遍历子树，遇到 `class`/`id` 小写后包含 `date` 或 `time`，或标签是 `<time>`，就取 `datetime` 属性（没有则取文本）跑 `dateValue`。先命中的赢；遍历中 `v == articleLink && v != n` 时直接 `return false`（**跳过文章链接自身的子树**，注释说「标题可能提到别的日期」）。
2. **整块文本兜底**（`parser.go:250-264`）：把子树文本拼成串（跳过 `articleLink` 子树、跳过 `script`/`style`），`clean` 后跑 `dateValue` 取第一个完整日期。

边界与误判面：

- `strings.Contains(class,"date")` 是**子串**判断：`update`、`candidate`、`metadata`、`validate` 都含 `date`；`strings.Contains(class,"time")` 同理命中 `lifetime`、`runtime`、`datetime`。任何把这类 class 用在容器上的模板，都会让「容器内第一个日期」被当成发布日期。
- 第 2 阶段是**语义盲**的：整行文本里只要出现第一个完整日期就返回，`【活动时间】2026-11-13`、`公示日期：2026年9月14日——2026年9月16日`、正文里的「2005年12月1起施行」都会被选中（第 3、4 节有真实后果）。
- `articleLink` 子树被跳过，本意是「别拿标题里的日期」，但副作用是**本行自己的日期也可能被跳过**（若日期写在 `<a>` 内部且没有 `class~date/time` 节点），于是结果退化为空——`cases2.out` F2/F3 就是这个结果。

### 1.4 `articleDate`（`parser.go:281-294`）

```go
for n, depth := a, 0; n != nil && depth < 5; n, depth = n.Parent, depth+1 {
    if n != a && (n.Data == "ul" || n.Data == "ol" || n.Data == "body" || countLinks(n) > 3) {
        break
    }
    if d, original := dateInNode(n, a); original != "" { return d, original }
    if n.Data == "li" || n.Data == "tr" { break }
}
```

向上最多 5 层，**先判 break 再看日期**（所以第 5 层 `countLinks>3` 时该层的 `dateInNode` 根本不会执行）；`li`/`tr` 层检查完即停。

聚合文本的范围随层数放大：到第 2~3 层时，`dateInNode` 跳过的是「文章链接」子树，**同一父节点下的其它行/其它块会被全部并入文本**。这是第 2 号 bug 的机制（第 3.2 节给出逐层证据）。

### 1.5 `detailMetadata`（`parser.go:687-748`）

- meta 阶段（`parser.go:688-722`）：`name`/`property` 含 `articletitle`/`og:title` → 标题；含 `pubdate`/`published`/`publishdate`/`datepublished` → 日期（且**只接受 `dateValue` 返回 date 非空的结果**，`parser.go:701`）。
- 标题阶段（`parser.go:706-720`）：`h2/h3/h4` 若祖先 3 层内 class 含 `art-tit`/`cont-tit`/`news-title`/`article-title` 视为文章标题；此外**任何 `h1`**、`class` 含 `arti_title` 的节点也收。
- 日期回退（`parser.go:727-746`）——本报告的核心缺陷：

```go
key := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
t := textContent(n)
if len(t) > 1000 { return true }
if strings.Contains(key, "date") || strings.Contains(key, "time") || strings.Contains(key, "info") ||
   strings.Contains(t, "发布时间") || strings.Contains(t, "发布日期") || strings.Contains(t, "发表时间") ||
   ((strings.Contains(t, "时间：") || strings.Contains(t, "时间:")) && strings.Contains(t, "浏览")) {
    if d, o := dateValue(t); d != "" { date, original = d, o; return false }
}
```

`strings.Contains(key,"info")` 会把 `infotitle`（VSB 通用类名，**文章标题容器**）、`newsinfo`（发布时间容器）、`article-info`、`infobox` 等一律看作「日期容器」；而 `dateValue(t)` 又对标题文本做无边界搜索。两者叠加 = 标题里的任何日期都可能是「发布时间」。

边界与缺口：

- `len(t) > 1000` 只是「不往下走」，**不是剪枝**：外层大容器仍会被 `textContent` 全量算一遍（`perf.out`：146KB 页面单次 `detailMetadata` 2.09ms，120KB 页面 4.30ms，测试环境可接受，但复杂度是「元素数 × 子树规模」）。
- 回退分支不校验候选节点是否是「发布信息」：`<div class="foot-time">更新时间：2026-09-14</div>` 会命中（`cases5.out` P1）。
- 未覆盖的 meta：`dcterms.created`（Dublin Core 常见）不匹配任何关键词（`cases5.out` P6 输出空）。
- 没有 `<time>` 处理：`<article><time datetime="2026-09-14T08:00:00+08:00">` 且父容器 class 不含 date/time 时识别不到（P7）。

**实测「哪个节点赢」**（`trace` 子命令，`trace-1571.out`）：

```
-> <div class="infotitle" id=""> [key~info]
     lenText=40 dateValue="2026-12-31" original="2026年12月31日"
     text="古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）"
detailMetadata => title="" date="2026-12-31" text="2026年12月31日"
```

---

## 2. 已确认 bug 1：详情页把标题里的「试用有效期」当发布时间

### 2.1 它从哪个列表页来、列表行长什么样

**列表页**：`https://lib.cdu.edu.cn/zy/dzzy/sjk1.htm`（图书馆「电子资源 / 数据库」栏目；快照里该 channel 记为 `资讯动态`，`data/snapshot.json:6055`）。这一页**不是**从首页 `parseChannels` 直接发现的，而是栏目页里再发现的二级栏目（`crawler.go:258-260` 每页首页再 `addChannels`）。

**列表行 HTML**（`research/tmpreview-crawler/cache/lib.cdu.edu.cn_zy_dzzy_sjk1.htm.html:270-272`，本次真实抓取）：

```html
        <li class="space"><span class="title">
            <a href="../../info/1057/1571.htm" target="_blank" title="古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）">古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）</a>
           </span><span class="date">浏览：<span id="dynclicks_u8_1571" name="dynclicks_u8_1571"></span></span></li>
```

关键点：`<span class="date">` 里**只有「浏览：」和动态点击数占位符，没有日期**。所以列表页解析结果是 `date=""`（`probe` 对该页跑 `parseArticles`：19 条全部 `DATE="" TEXT=""`，标题带「试用有效期」原样保留）。→ 这正是为什么该条**会**进入详情补全队列（`crawler.go:267` 的 `Date == ""` 成立），随后在详情页被 bug 1 写错。**列表页不是错值来源，详情页才是**——这一点必须与 bug 2（列表页错值）严格区分。

同一列表页还有 4 条结构完全相同的行（`1543 / 1540 / 1551 / 1542`），标题里都带「试用有效期…2026年…」，因此该 bug 在这一个页面上就有 5 个潜在受害者，实际中招 3 个（`detailBudget=12` 截断，见 bug 4）。

### 2.2 事实与证据

- 结论条目：`data/snapshot.json:6049-6059`
  - `title`: `古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）`
  - `date`: `2026-12-31`，`dateText`: `2026年12月31日`，`channel`: `资讯动态`
- 详情页原文（`research/tmpreview-crawler/cache/lib.cdu.edu.cn_info_1057_1571.htm.html:224-227`，2026-10-21 重新抓取，与此前一致）：

```html
<div class="infotitle">古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）</div>
<div class="bodycon">
  <div class="newsinfo">来源：
    发布时间：2022-05-16  编辑：技术部    浏览数：<script>_showDynClicks("wbnews", 1791171672, 1571)</script> ...</div>
</div>
```

- 该页**没有** `meta[name=PubDate]`/`og:title`，也没有 `h1`、`class` 含 `art-tit` 的容器（`grep` 统计：`h1=0 og/ArticleTitle=0 artTitClass=0`）。于是 `detailMetadata` 只能走 `parser.go:727-746` 的回退。
- 回退遍历是文档序：`div.infotitle` 在 `div.newsinfo` **之前**，前者的 class 含 `info` 被选中，`dateValue("…（试用有效期：即日起--2026年12月31日）")` 返回 `2026-12-31`，非空 → 立即 `date = 2026-12-31; return false`，**再也不会读到 `发布时间：2022-05-16`**。
- A/B 对照（同一份真实 HTML，只删 `div.infotitle`）：

```
=== 原始（有 infotitle） ===
detailMetadata => title="" date="2026-12-31" text="2026年12月31日"
=== 删掉 infotitle 后 ===
-> <div class="bodycon" id=""> [text~发布时间]
     dateValue="2022-05-16"
detailMetadata => title="" date="2022-05-16" text="2022-05-16"
```

  即**在 1571 这一页，唯一根因就是 `infotitle` 抢在 `newsinfo` 前面命中 `key~info`**。（同模板的 1540/1543 由外层 `div.pagew` 胜出，见下两例——同一条件、不同粒度。）

- 同机制第二例 `lib/info/1057/1540.htm`（快照 `data/snapshot.json:6063-6073`，`date=2026-12-31`）：页面同样有 `infotitle`（`Mirror（米乐）心理资源服务平台（试用有效期：即日起--2026年12月31日）`，缓存 `:224`）与 `发布时间：2022-03-01`（缓存 `:226-227`）。**该页胜出的不是 `infotitle`，而是更外层的 `div.pagew`**（`trace-1540.out`）：

  ```
  -> <div class="pagew" id=""> [text~发布时间]
       lenText=892 dateValue="2026-12-31" original="2026年12月31日"
       text="资源 / RESOURCES 纸质资源… 电子资源 数据库… 成都研究 Mirror（米乐）心理资源服务平台（试用有效期：即日起--2026年…"
  detailMetadata => title="" date="2026-12-31" text="2026年12月31日"
  ```

  外层容器文本含「发布时间」四字（来自 `newsinfo`），且 `len(t)=892 ≤ 1000` 未被剪掉，于是 `dateValue(整块文本)` 取到**文档序更早的标题日期**。删掉 `infotitle` 后该容器文本里再无 2026-12-31，输出回落为 `2022-03-01`。→ 同一 `parser.go:739` 条件在「内层节点」与「外层容器」两种粒度上都会误命中，这是修 bug 1 时必须一并处理的（只改 `key~info` 关键字不够）。
- **第三例（本次已抓取确认）** `lib/info/1057/1543.htm`：真实页面 `infotitle` = `广益多媒体外语学习平台（试用有效期：即日起--2026年3月1日）`（`research/lib-detail-1543.html:224`），真实发布 `发布时间：2022-03-08`（同文件 `:227`）。该页 `trace-1543.out`：

  ```
  -> <div class="pagew" id=""> [text~发布时间]
       lenText=813 dateValue="2026-03-01" original="2026年3月1日"
  detailMetadata => title="" date="2026-03-01" text="2026年3月1日"
  ```

  快照 `date=2026-03-01`、`dateText="2026年3月1日"`（`dateText` 是标题子串——全量统计里只有 3 条这种「dateText ∈ title」，即 1571 / 1540 / 1543）。**应输出 `2022-03-08`**。因为 `2026-03-01` 早于抓取日 2026-09-14，用「未来日期」过滤器筛不出来，必须靠「dateText ∈ title」或「详情页发布时间比对」才能发现。
- 抓取可复核性：`research/lib-detail-{1571,1540,1543}.html` 与 `research/tmpreview-crawler/cache/lib.cdu.edu.cn_info_1057_*.htm.html` 中 1571 / 1540 两份 SHA-256 完全相同（两次独立抓取），说明该页今日仍是此结构，问题可稳定复现。

### 2.3 最小复现（可直接作为 `parser_test.go` 用例）

```go
// 期望 2022-05-16，实际 2026-12-31
func TestDetailMetadataPrefersPublicationTimeOverTitleValidityPeriod(t *testing.T) {
	body := `<html><body><form>
<div class="infotitle">古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）</div>
<div class="bodycon">
  <div class="newsinfo">来源：
    发布时间：2022-05-16  编辑：技术部    浏览数：<script>_showDynClicks("wbnews", 1791171672, 1571)</script></div>
</div>
<div id="vsb_content" class="bodycon"><div class="v_news_content">
  <p>试用期限：即日起--2026年12月31日</p>
</div></div>
</form></body></html>`
	_, date, text := detailMetadata(parseFixture(t, body))
	if date != "2022-05-16" || text != "2022-05-16" {
		t.Fatalf("got date=%q text=%q, want 2022-05-16（发布时间）", date, text)
	}
}
```

实测输出（`cases.out` B 段）：

```
实际: title="" date="2026-12-31" text="2026年12月31日"
期望: date="2022-05-16" (发布时间：2022-05-16)

-- 对照组：把 class="infotitle" 改成 class="art-tit"，其余不变 --
实际: date="2026-12-31"    ← 说明不是「infotitle 特例」，而是 article-title 类容器只要绕开 info 仍会命中 textContent 里的日期
-- 对照组2：只把 newsinfo 移到 infotitle 之前（仅调文档序） --
实际: date="2022-05-16"    ← 一击即中，确认根因是「文档序 + key~info」
-- 对照组3：<div class="infotitle">有效期至2026年12月31日</div> + <div class="newsinfo">发布时间：2022-05-16</div>
实际: date="2026-12-31"
```

> 说明：对照组 1 仍错，是因为 `art-tit` 里没有结构化日期，兜底扫到标题文本；这说明**只删掉 `info` 关键词不够**，还需要「优先取真正的发布时间标记」的排序（见第 7 节修复方向）。

### 2.4 影响面

- 快照全量：`date` 晚于抓取日的只有 2 条，正是 1571 与 1540；再加 1543 这条「过去日期」共 3 条确定错值。
- 潜在面：任何「标题里带有效期/届次年份/活动年份」的详情页（资料库试用公告、会议通知、评奖届次），只要该页缺 `PubDate` meta 且 `infotitle` 类容器先出现，就会重演。
- 下游影响：`internal/service/service.go:231-234` 只在 `a.Date==""` 时继承旧值，所以一旦写错，后续同步会**一直保留错值**（错值不会被「新抓取为空」冲掉）。

---

## 3. 已确认 bug 2：`articleDate` 向上聚合把「兄弟行摘要日期」当本行发布时间

### 3.1 真实结构与证据

`https://journal.cdu.edu.cn/`（期刊中心首页）「政策法规」区是**三条平铺的 `div.s4-con`**，行内**完全没有日期节点**（`research/journal.html:698-763`）：

```html
<div class="s4-main"><div>
  <div class="s4-con wow fadeInLeft">
    <a href="info/1058/1332.htm" tabindex="-1">
      <h4 class="over eclip">《期刊出版管理规定》</h4>
      <p class="overs eclip2">（新闻出版总署令第31号发布，2005年12月1起施行）…</p>
    </a>
  </div>
  <div class="s4-con wow fadeInLeft">
    <a href="info/1058/1333.htm" tabindex="-1">
      <h4 class="over eclip">《高等学校学报管理办法》</h4>
      <p class="overs eclip2">高等学校学报管理办法 （教育部1998年4月1日公布，1998年5月1日起实行）…</p>
    </a>
  </div>
  <div class="s4-con wow fadeInLeft">
    <a href="info/1058/1334.htm" tabindex="-1">
      <h4 class="over eclip">教育部、国家新闻出版广电总局关于进一步加强和改进高校出版...</h4>
      <p class="overs eclip2">教育部 国家新闻出版广电总局关于进一步…（无日期）</p>
    </a>
  </div>
</div></div>
```

用 `chain` 子命令复刻 `articleDate` 的祖先链（`chain-journal1332.out`）：

```
target <a href="info/1058/1332.htm">
articleDate => date="1998-04-01" original="1998年4月1日"
depth=0 <a> countLinks=1 dateInNode=(""，"")        ← 自己行的 a，dateInNode 跳过 articleLink 子树 → 空
depth=1 <div class="s4-con ..."> dateInNode=(""，"") ← 仍然只有本行文本（被跳过）→ 空
depth=2 <div class=""> countLinks=3 dateInNode=("1998-04-01","1998年4月1日")
     聚合文本="《期刊出版管理规定》（…2005年12月1起施行）… 《高等学校学报管理办法》（教育部1998年4月1日公布，1998年5月1日起实行）…"
depth=3 <div class="s4-main"> 同上
depth=4 <div class="px1480"> countLinks=4 stopHere=true  ← 若走到这层才 break
```

- 关键：第 2 层 `countLinks=3`，`countLinks(n) > 3` 不成立 → **不 break**；`dateInNode` 跳过「本行 `<a>`」，于是文本里剩下的是**兄弟行**内容，第一个完整日期是 1333 行的 `1998年4月1日`。
- 与快照完全吻合（`data/snapshot.json`）：`1332.htm → 1998-04-01 / 1998年4月1日`、`1333.htm → 2005-12-01 / 2005年12月1`、`1334.htm → 2005-12-01 / 2005年12月1`。三条互相错位，且 1334 **本来没有任何日期**却拿到了 1333 行里的 2005-12-01。
- 真实值：`journal.cdu.edu.cn/info/1058/1332.htm` 详情页写 `时间：2015-12-02`（缓存 `journal.cdu.edu.cn_info_1058_1332.htm.html:340`），`detailMetadata` 对该页返回 `("《期刊出版管理规定》", "2015-12-02", "2015-12-02")`（`whichjournal.out`）。但因为列表已给出非空 `1998-04-01`，该条不会进入详情补全队列（见 bug 4），错值固化。

### 3.2 最小复现

```go
// 期望三条都没有 date（列表未渲染日期）；实际 1998-04-01 / 2005-12-01 / 2005-12-01
func TestArticleDateDoesNotLeakFromSiblingRows(t *testing.T) {
	body := `<div class="s4-main"><div>
<div class="s4-con"><a href="info/1058/1332.htm"><h4>《期刊出版管理规定》</h4><p>（新闻出版总署令第31号发布，2005年12月1起施行）</p></a></div>
<div class="s4-con"><a href="info/1058/1333.htm"><h4>《高等学校学报管理办法》</h4><p>（教育部1998年4月1日公布，1998年5月1日起实行）</p></a></div>
<div class="s4-con"><a href="info/1058/1334.htm"><h4>关于进一步加强和改进高校出版…</h4><p>各省、自治区、直辖市教育厅…</p></a></div>
</div></div>`
	items := parseArticles(parseFixture(t, body), "https://journal.cdu.edu.cn/", model.Source{ID:"journal"}, "政策法规", "")
	for _, it := range items {
		if it.Date != "" {
			t.Errorf("sibling summary leaked as publication date: %+v", it)
		}
	}
}
```

实测（`cases2.out` F4 段）：

```
date="1998-04-01" text="1998年4月1日"   title="《期刊出版管理规定》"
date="2005-12-01" text="2005年12月1"    title="《高等学校学报管理办法》"
date="2005-12-01" text="2005年12月1"    title="关于进一步加强和改进高校出版..."
```

### 3.3 触发条件归纳（这类结构在 cdu 站点很常见）

1. 列表行**不渲染发布日期**（只渲染标题+摘要），或日期写在 `<a>` 内且没有 `class~date/time` 节点；
2. 同一父容器下有 ≥2 个兄弟 `<li>`/`<div>`，且 `countLinks(父) <= 3`（3 个链接是**允许**的，`>3` 才 break）；
3. 任一兄弟行的标题/摘要含完整日期。

真实页面同时满足 1+2+3 的已确认只有 `journal` 首页政策法规区；`jxgc` 列表行因为整行包在 `<a>` 里且日期块 class 含 `date`，反而不触发（`rowscan.out` 只在 journal 报了 date 冲突，jxgc 命中的是标题残留日期问题，见 bug 5）。

---

## 4. 已确认 bug 3：行内摘要里的活动日期被当发布时间

`parser.go:250-264` 的兜底聚合会读整行文本（跳过 articleLink 子树）。当摘要**不在 `<a>` 内**时，摘要里的活动日期就成了「发布日期」。

```go
// 期望 date="" text="09-14"；实际 date="2026-11-13"
body := `<ul>
<li><a href="info/1046/12201.htm" title="关于举办2026年实验室安全培训的通知">关于举办2026年实验室安全培训的通知</a>
  <span class="summary">本次培训将于2026年11月13日举办，请各单位组织人员参加。</span>
  <span>09-14</span></li>
</ul>`
```

实测（`cases.out` C 段）：`date="2026-11-13" text="2026年11月13日"`。这类 `<li><a>标题</a><p class="desc">摘要</p><span>月日</span></li>` 是 VSB 站点的典型形态，日期行反而被 `partialDateRE` 只保留月日而丢弃。

同源问题在详情页的版本见 `cases5.out` P2：

```html
<div class="content">时间：2026-11-13 浏览：120</div>
→ date="2026-11-13"   （命中 parser.go:739 的「时间：+浏览」组合，但这是活动时间）
```

——所以 `parser.go:739` 的「时间：+浏览」启发式在正文/活动信息块上同样会误判。

---

## 5. 已确认 bug 4：详情补全预算让「非空错值」永久固化

`crawler.go:264-277`：

```go
indices := make([]int, 0)
for i, article := range articles {
    if article.Date == "" || strings.Contains(article.Title, "...") || strings.Contains(article.Title, "…") {
        indices = append(indices, i)
    }
}
...
if len(indices) > detailBudget { indices = indices[:detailBudget] }  // detailBudget = 12
```

- 只有 `Date == ""`（或标题被截断）的条目才会去抓详情页。**一旦列表页给出了错误但非空的日期，详情页的真实发布时间永远没机会覆盖它**。
- 三处实证：

| URL | 快照 date（错） | 详情页真实值 | 是否进补全队列 |
|---|---|---|---|
| `lib.cdu.edu.cn/info/1057/1571.htm` | 2026-12-31 | `发布时间：2022-05-16` | 是（列表无日期）→ 但回退分支取错（bug 1） |
| `lib.cdu.edu.cn/info/1057/1540.htm` | 2026-12-31 | `发布时间：2022-03-01` | 是 → 同样取错 |
| `lib.cdu.edu.cn/info/1057/1543.htm` | 2026-03-01 | `发布时间：2022-03-08`（`research/lib-detail-1543.html:227`） | 是 → 但回退分支取到标题有效期（bug 1） |
| `journal.cdu.edu.cn/info/1058/1332.htm` | 1998-04-01 | `时间：2015-12-02` | **否**（Date 非空）→ 永远不会被纠正 |

- 顺带证实 `detailBudget=12` 的实际效应：`lib/zy/dzzy/sjk1.htm` 该页 19 条，按文档序第 1~12 条全部有 date，第 13~19 条（1244/1551/1699/1720/1217/1542/1214）全部 `date=""`（`snapshot` + 独立脚本核对），与预算截断点完全对齐。
- `crawler.go:199-223` 的 `merge` 也是同向的：只有 `old.Date == ""` 才接受新日期；且首页（`首页动态`）先合并，栏目页的日期**不会**覆盖首页解析出的错值，只能覆盖 `Channel`。这正是 `xczgb/info/1036/1143.htm` 等条目 `channel` 停留在 `首页动态` 的原因。

---

## 6. 非 bug 的源站数据问题（必须与解析 bug 区分）

| URL | 快照 date | 页面原文 | 判定 |
|---|---|---|---|
| `xczgb.cdu.edu.cn/info/1036/1143.htm` | `1938-01-01` | `<div class="art-tit cont-tit"><h3>抗日战争时期</h3><p><span>发布日期：1938-01-01</span>…`（缓存 `:175-181`） | **源站标注**。页面自己把「抗日战争时期」这条历史条目的发布日期写成 1938-01-01；`detailMetadata` 忠实返回。不是解析 bug |
| `xczgb.cdu.edu.cn/info/1036/1140.htm` | `1922-01-01` | 首页日历块 `<p>1922</p>` + 详情页「发布日期」 | 源站标注（同年份系列） |
| `xczgb.cdu.edu.cn/info/1036/1141.htm` | `1925-10-01` | 同上 | 源站标注 |
| `xczgb.cdu.edu.cn/info/1036/1142.htm` | `1927-08-01` | `发布日期：1927-08-01`（缓存 `:178`） | 源站标注 |
| `xczgb.cdu.edu.cn/info/1036/1144.htm` | `2002-11-01` | 首页 `<p>2002</p>` | 源站标注 |
| `xczgb.cdu.edu.cn/info/1036/1151.htm` | `2012-11-01` | 首页 `<p>2012</p>` | 源站标注 |
| `dfl.cdu.edu.cn/info/1025/11546.htm` | `2002-03-03` | `<div class="author"><span>时间：2002-03-03 05:16</span>`（`pages3.out` 抓到的原文） | **源站标注**（招聘启事的登记时间），与解析无关 |
| `journal.cdu.edu.cn/info/1058/1332.htm` | `1998-04-01` | 详情页 `时间：2015-12-02`，正文含「2005年12月1起施行」 | **解析 bug**（bug 2）——快照值来自兄弟行摘要，不是源站标注 |
| `journal.cdu.edu.cn/info/1058/1334.htm` | `2005-12-01` | 页面无任何日期 | **解析 bug**（bug 2） |
| `nic.cdu.edu.cn/info/1038/11xx.htm` 等 | 2000-09-20 / 2000-10-08 / 2000-12-28 / 2005-01-28 | 政策法规原文 | 源站体例（收录法规的原始公布时间），非 bug |

**判定建议（写进「新旧快照差异」比对规则）**：新抓取结果与旧快照的日期差异，按下面顺序分类，不要一律当回归：

1. 页面 HTML 里存在 `发布时间/发布日期/发表时间/时间：` 或 `meta[PubDate]`，且新值 = 该标记值 → **修正，采纳**；
2. 页面里找不到任何发布时间标记，但新值/旧值出现在标题或正文摘要里 → **解析 bug，丢弃该日期**（`date=""` + 保留 `dateText`）；
3. 页面标记值与快照值一致，只是年份离谱（1938/1922/2002…）→ **源站数据问题，保留并标注**（可加一层「源站标注」标记，不要用「未来/过去年份」过滤，否则 1543 的 2026-03-01 这类会漏）；
4. 新值为空而旧值非空 → 检查是否 `detailBudget` 截断导致（`Date==""` 的条目超过 12 条时必然发生），不要当作「源站删除了日期」。

---

## 7. 其他已确认问题（按严重度）

### 7.1 标题残留日期（`parser.go:646-649`）

```go
if original != "" {
    title = clean(strings.TrimSuffix(strings.TrimPrefix(title, original), original))
}
```

只裁剪 `original`（即 `dateValue` 命中的那一段原文）这一种写法。当同一行有两种格式时（`<a>` 的 `title` 属性里是 ISO 日期、行内日期块是分块格式），ISO 残留：

```
输入：<a href="info/1046/12201.htm" title="成都大学科学技术项目协作外拨事项公示 2026-09-14">
        <div class="pull-left txt-date text-center"><span>14</span><br>2026-09</div>…
输出：date="2026-09-14" text="14 2026-09" title="成都大学科学技术项目协作外拨事项公示 2026-09-14"
期望：title 不含日期
```

- 快照实证 34 条，**全部来自机械工程学院**（`jxgc.cdu.edu.cn`），例如
  `https://jxgc.cdu.edu.cn/info/1046/12201.htm` → `成都大学科学技术项目协作外拨事项公示 2026-09-14`；
  `https://jxgc.cdu.edu.cn/info/1025/12000.htm` → `【通知公告】机械工程学院2026年考研奖学金第二批次获奖名单公示 2026-09-14`。
  这些条目的 `dateText` 都是分块格式（`14 2026-09` 等 22 种），而标题末尾是 ISO——两种写法并存，只裁掉分块那种。
- 另有一类标题污染来自站点自己把日期写进标题：`shpg.cdu.edu.cn/info/1002/23xx.htm` 共 8 条 `【公告标题】…【公告时间】2026年09月08日【结束时间】…`——这是**源站的标题排版**（列表标题就是拼接串），不是解析器加的，修复方式是详情补全标题（但同样受 `detailBudget` 限制）。

### 7.2 非文章 URL 被当文章（`parser.go:640-645` + `excludedTree:372-381`）

```go
if !sameSite(raw, base) && (!articleURL(raw) || original == "") { return false }
if !articleURL(raw) && original == "" { return false }
```

只要所在行能聚合出 `original`，**任何链接**都放行——包括 `index.htm`、`bszn.htm` 这种导航页。`excludedTree` 只排除 `footer`/`script`/`style`/`noscript` 和 class/id 含 `footer`/`friendlink` 的节点，管不到 `<div class="position">`（面包屑）、`<div class="copyright">`、`<div class="foot">`。

最小复现（`cases4.out` O1）：

```html
<div class="position"><a href="index.htm">首页</a><span>2026-07-14</span></div>
→ 收录 title="首页" url="https://zzb.cdu.edu.cn/index.htm" date="2026-07-14"
```

快照实证：86 条非 `/info/` 形态的 article，其中 15 条是导航页（`index.htm`/`xzzq1.htm`/`gzzd1.htm`/`bszn.htm`/`yjsjy.htm`/`gs.htm`…），如
`https://zzb.cdu.edu.cn/index.htm`（title=首页）、`https://dfl.cdu.edu.cn/yjsjy.htm`（title=研究生教育）、`https://xzbgs.cdu.edu.cn/xzzq1.htm`（title=首页）。

### 7.3 `parseChannels` 丢栏目（`parser.go:603-609`）

```go
if a.Parent != nil && (a.Parent.Data == "li" || a.Parent.Data == "tr" || countLinks(a.Parent) == 1) {
    if _, date := dateInNode(a.Parent, a); date != "" { return false }
}
```

本意是「别把有日期的行当栏目入口」，但栏目导航里经常在同一 `<li>` 里挂一个「最新日期」角标：

```
输入：<li><a href="tzgg.htm">通知公告</a><span>09-14</span></li>
输出：CH="新闻动态" https://x.cdu.edu.cn/xwdt.htm      ← 「通知公告」栏目被丢弃
```

（`cases.out` E 段 / `cases2.out` I 段。）真实站点是否踩到取决于模板；当前 lib/xczgb/journal 三站的栏目导航没在同行放日期，所以暂未观测到真实漏栏目，但这是**已知可触发**的行为。

### 7.4 `detailMetadata` 标题识别（`parser.go:716`）

`n.Data == "h1"` 收任何 `h1`，不管它在不在正文区。实测（`cases.out` D 段）：

```
<h1>成都大学信息网络中心</h1><div class="art-tit"><h3>关于校园网维护的通知</h3></div>
→ title="成都大学信息网络中心"     （应为「关于校园网维护的通知」）
```

真实 cdu 详情页多数用 `art-tit/cont-tit + h3`（`xczgb`、`journal`、`jw` 均如此，实测标题正确），但也有页面 `h1` 在页头。另外 `og:title`/`ArticleTitle` 在这三站**全部缺失**（grep 结果均为 0），所以 meta 分支实际上很少生效。

### 7.5 抓取层

| 项 | 位置 | 结论 |
|---|---|---|
| charset 处理 | `crawler.go:123` `charset.NewReader(strings.NewReader(string(body)), contentType)` | `[]byte→string→[]byte` **不丢字节**（`charsetprobe.out` [5]：长度一致、逐字节相等），所谓「二次编码」不成立；真正的问题是 `charset.NewReader` 的探测顺序：**响应头 charset 优先**，其次读前 1024 字节找 `<meta charset>`。实测 [2] meta 被挤到 1024 字节之后 → 整页乱码 `³É¶¼´óÑ§Í¼Êé¹Ý…`；[3] 头写 `charset=utf-8` 但实际 GBK → 乱码。当前 cdu 三站均为 `Content-Type: text/html`（**无 charset**）+ 头部 `meta charset=utf-8`（`hdr.out`），落在正确路径上，属**当前不触发、但换站即炸** |
| 4 MiB 限制 | `crawler.go:24,116-122` | `LimitReader(maxBodyBytes+1)` 后判 `len > maxBodyBytes` 才报错，逻辑正确（能区分「刚好 4MiB」与「超限」）。实测最大页面 146KB，远未触及；但超限是**硬失败**（整个 channel 直接 `break`，`crawler.go:251-255`），大站点会整列丢 |
| 限速 | `crawler.go:39,60-78` | 按 host 排队，`interval=250ms`（同 host 4 QPS）；`nextRequest` map 无上限但 host 数有限，可接受 |
| 并发 | `crawler.go:39,90-95,278-314` | 全局信号量 8；`MaxConnsPerHost=3`；详情补全 3 个 worker。注意 `wait()` 在拿信号量**之前**，所以排队等待期间不占并发位，设计合理 |
| 重定向 | `crawler.go:45-56` | 最多 8 跳；离开 `*.cdu.edu.cn` 或被 `protectedURL` 拦截就报错。`final = response.Request.URL`（`:108`）作为后续相对链接的 base，正确 |
| 超时 | `crawler.go:44-45` | `ResponseHeaderTimeout=12s`、`Client.Timeout=18s`，对 cdu 站点足够（实测页面均在 1s 内） |
| nextPage | `parser.go:666-685` | 只认「下一页/下页/next/rel=next」，且要求同站、非文章 URL。lib 的 `sjk1.htm` 实测得到 `sjk1/6.htm`（VSB 反向分页），正确；`journal`/`xczgb` 首页无分页返回 `""`，正确 |
| merge 去重 | `crawler.go:199-223` | 以 URL 为键，先首页后栏目；`Date` 只填空不纠正（bug 4）；`Channel` 用栏目名覆盖 `首页动态`；标题去省略号（`...`/`…`）逻辑保留 |
| 错误聚合 | `crawler.go:315-331` | 有任何 warning 时 `status=partial` 且**返回 error**，调用方按错误处理；`warnings[:8]` 截断时用 `append(warnings[:8], ...)` 会**就地覆盖第 9 个元素**（`crawler.go:327`），属于小瑕疵（不影响正确性） |

**未触发的观察**：`fetch` 只接受 `Content-Type` 含 `html` 或 `text/plain`；`xczgb/xsgz/xsdt.htm` 和 `journal/zcfg.htm` 返回的是**nginx/WAF 的「页面正在修复中，请稍后重试」提示页**（13KB，`<title>结果</title>`，2026-10-21 抓取时两站同一份模板），HTTP 200。这类页面会被正常解析成 0 篇文章 → 源状态 `empty`，属于「站点反爬/维护」，不是解析 bug，但需要在比对时识别（否则会被当成「栏目被清空」）。

---

## 8. 测试缺口（`internal/crawler/parser_test.go`）

现有 9 个用例（`parser_test.go:25-312`）覆盖了目录解析、行内日期、MORE 链接、详情 meta 优先、跨站边界、嵌套栏目翻页、取消、真实站点回归、jxgc 摘要、短标题、长标题，但**上述 4 个已确认 bug 全部没有用例**：

| 缺口 | 应对应用例 |
|---|---|
| `infotitle` 在 `newsinfo` 之前的详情页 | 第 2.3 节的 `TestDetailMetadataPrefersPublicationTimeOverTitleValidityPeriod` |
| 兄弟行摘要日期串行 | 第 3.2 节的 `TestArticleDateDoesNotLeakFromSiblingRows` |
| 「非空错值不进补全队列」 | 需要 `CrawlSource` 级用例（`fixtureTransport` 已有：`parser_test.go:140-175`），断言「详情页 PubDate 与列表不同时以详情为准，或列表值被标记为低置信」 |
| 标题残留第二种日期格式 | 第 7.1 节的输入片段，断言 `title` 不含 `2026-09-14` |
| `detailBudget` 截断 | `fixtureTransport` 造 13 条无日期文章，断言只有前 12 条被补全（当前行为），或断言「全部补全/预算可配」 |
| `parseChannels` 同行日期 | 第 7.3 节的输入片段 |
| `dcterms.created` / `<time datetime>` | `cases5.out` P6/P7 |
| charset 回退 | 可用 `charsetprobe` 的 GBK 双 meta 场景做单测 |

---

## 9. 建议修复方向（不改代码，仅方向）

1. **明细日期按「结构化标记」排序取用**（修 bug 1）：把回退分支的判定拆成三级证据——
   ① `meta[PubDate/published/datePublished/dcterms.created]`；
   ② 文本含 `发布时间/发布日期/发表时间` 的**最小**节点；
   ③ `class/id` 精确等于 `date|time|pubdate|newsinfo|author|info` 的节点；
   最后才用宽泛 `strings.Contains(key,"info")` + 全文搜索，并且**在候选多个时取「文本最短、离正文最近」的那个**，而不是文档序第一个。同时把 `len(t) > 1000` 从「不往下走」改成真正的剪枝（超过阈值直接 `return false`）。
2. **`articleDate` 禁止跨行聚合**（修 bug 2）：聚合文本必须限制在「同一个 `li`/`tr`/最内层行容器」内，或者干脆只用 `dateInNode` 的显式类名结果 + 行内文本（`li`/`tr` 内、排除兄弟 `li`）。当前「跳过 articleLink 但吞并兄弟行」的语义要改成「跳过 articleLink **且不进入兄弟行**」。
3. **日期允许多源冲突 + 置信度**（修 bug 3/4）：`Article` 增加 `dateSource`（`meta` / `publish-marker` / `row-text` / `none`）与 `dateConfidence`；详情页结果**始终允许覆盖**列表值（或至少当列表来源是 `row-text` 时覆盖），不再只对空值补全；`detailBudget` 从「12 条全局」改为「按来源优先级 + 可配置」。
4. **标题清洗**（修 bug 5）：用 `dateValue` 命中的 `original` 之外，再对标题做一次「去除首尾日期样式」的正则清洗（`^(?:\d{4}[-/.]\d{1,2}[-/.]\d{1,2}|\d{4}年\d{1,2}月\d{1,2}日)\s*` 与 `\s*\d{4}[-/.]\d{1,2}[-/.]\d{1,2}$`），或直接用详情页 `art-tit/h1` 覆盖标题并记录 `titleSource`。
5. **文章资格判定收紧**（修 bug 6）：`original != ""` 不应单独放行非 `articlePathRE` 的 URL；建议要求 `articleURL(raw) || (sameSite && pageNavigation(raw)==false && original != "")`，并把 `excludedTree` 扩展到 `class/id` 含 `position|breadcrumb|crumb|copyright|foot|nav|menu|sidebar` 的节点。
6. **源站数据问题与解析问题分离**：在快照里为每条 article 记录 `dateSource`，前端/比对脚本据此标注「源站标注」；不要用「未来日期」或「年份早于 2015」这类启发式过滤（会漏掉 1543 的 2026-03-01，也会误伤 xczgb 的历史条目）。
7. **回归护栏**：把本报告第 2.3、3.2、7.1、7.3 节的最小复现直接加入 `parser_test.go`；`journal.html`、`lib` 两个详情页、`jxgc_tzgg.html` 都已作为真实 fixture 存在（`research/journal.html`、`research/tmpreview-crawler/cache/lib.*`、`research/jxgc_tzgg.html`），可直接引用。

---

## 附：本报告所有证据文件索引

| 证据 | 路径 |
|---|---|
| 真实页面缓存（lib/xczgb/journal） | `research/tmpreview-crawler/cache/*.html` |
| 1571 详情页节点定位 | `research/tmpreview-crawler/trace-1571.out` |
| 1540 / 1543 详情页节点定位（pagew 胜出） | `research/tmpreview-crawler/trace-1540.out`、`trace-1543.out` |
| 独立抓取的 lib 详情页（可与 cache 互证） | `research/lib-detail-1571.html`、`lib-detail-1540.html`、`lib-detail-1543.html` |
| 1571/1540 去掉 infotitle 的 A/B | `research/tmpreview-crawler/patched-1571-no-infotitle.html`、`patched-1540-no-infotitle.html` |
| journal 祖先链逐层 | `research/tmpreview-crawler/chain-journal1332.out` |
| 正则边界 | `research/tmpreview-crawler/cases.out`（A/B/C/D/E） |
| 聚合与 excludedTree 边界 | `research/tmpreview-crawler/cases2.out`（F/G/H/I） |
| 标题残留 + merge 语义 | `research/tmpreview-crawler/cases3.out`（J/K/L/M） |
| 标题/导航污染 | `research/tmpreview-crawler/cases4.out`（N/O） |
| 详情回退边界 + meta 缺口 | `research/tmpreview-crawler/cases5.out`（P） |
| charset 探测 | `research/tmpreview-crawler/charsetprobe.out`、`hdr.out` |
| 真实站点响应头 | `research/tmpreview-crawler/hdr.out` |
| 性能 | `research/tmpreview-crawler/perf.out` |
| 快照全量统计 | `research/tmpreview-crawler/snapshot-analysis.txt` |
| 副本一致性 | `research/tmpreview-crawler/verify.out` |
