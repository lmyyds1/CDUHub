# 团队审查总览与日期修复说明

本次工作分两条线：**四人并行代码审查**（爬虫 / 后端 / 前端 / 数据质量），以及**日期识别错误的定位与修复**。
四份原始报告：

| 报告 | 覆盖范围 | 规模 |
| --- | --- | --- |
| [crawler.md](crawler.md) | `internal/crawler/`（parser.go、crawler.go）逐函数 + 真实站点复现 | 562 行 |
| [backend.md](backend.md) | `internal/service/`、`internal/server/`、`cmd/`、Wails 入口 | 38.5 KB |
| [frontend.md](frontend.md) | `frontend/src/`（日期展示/筛选、搜索性能、React 正确性） | 31.8 KB |
| [dates.md](dates.md) + [date-scan.py](date-scan.py) | `data/snapshot.json` 全量日期质量（16753 篇、73 来源、93 个真实 URL 证据） | 39.6 KB |

---

## 一、你报的日期错误：已修复

**现象**：列表顶部两条图书馆文章显示 `2026-12-31`，实际发布时间是 `2022-05-16` / `2022-03-01`。

**根因**（`internal/crawler/parser.go`，修复前 `detailMetadata` 的回退分支）：

1. 标题容器 `<div class="infotitle">古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）</div>`
   的 class 含 `info`，被旧的 `strings.Contains(key,"info")` 判成「日期容器」；
2. 遍历是文档序，`infotitle` 在真正的 `<div class="newsinfo">发布时间：2022-05-16</div>` **之前**，
   于是 `dateValue("…2026年12月31日")` 一旦取到就 `return`，真实的发布时间再也没机会被读到。
3. 该列表页 `<span class="date">` 里只有「浏览：」占位符，列表阶段 `date=""`，所以这条**必然**进入详情补全队列，
   然后在详情页被写错。同页还有 `1540`（应 2022-03-01）、`1543`（应 2022-03-08）同样中招。

**修复后的取值规则**（`parser.go` 新增 `publicationDate / nonPublicationDate / titleLikeNode / labelledDateIn / dateStampIn`）：

- 语义过滤：日期前 16 个字符内出现「有效期 / 截止 / 将于 / 举办 / 报名 / 考试 / 开馆 / 闭馆」时，**不认定为发布日期**；
  若整页只有这类语义日期，宁可不填日期（`Date=""` + 保留 `dateText`），也不误填。
- 详情页候选分级：① 含「发布时间/发布日期/发表时间」标记的节点 → ② class/id 像元数据（date/time/info）的节点 →
  ③ 标题容器（VSB 的 `.art-tit` 会把标题和日期包在一起）。同级取**文本最短**的节点，即最贴合该日期的那一格。
- 顺带修掉的两个同类问题（都有真实案例）：
  - 列表页 `articleDate` 向上 5 层聚合时会把**兄弟行摘要**里的日期当本行发布日期
    （`journal.cdu.edu.cn` 三条政策法规被记成 1998-04-01 / 2005-12-01，实际当天无日期）；
    现在遇到含多个链接的容器即停止，聚合时跳过外来 `<a>` 子树。
  - 发布日期**写在文章锚点内部**（`jxgc` / `bs` / `sqjsb` 的 `<span>2026-09-15</span>`、`<font>2023-04-10</font>`）
    时，日期混进标题且记录无日期；现在会识别「整格纯日期」与「标题尾部日期」并把日期剥离出来。

**验证（真实站点、真实快照重抓）**

| 用例 | 修复前 | 修复后 |
| --- | --- | --- |
| `lib.cdu.edu.cn/info/1057/1571.htm` | 2026-12-31 | **2022-05-16** |
| `lib.cdu.edu.cn/info/1057/1540.htm` | 2026-12-31 | **2022-03-01** |
| `lib.cdu.edu.cn/info/1057/1543.htm` | 2026-03-01 | **2022-03-08** |
| `journal.cdu.edu.cn/info/1058/1332.htm` | 1998-04-01 | **2015-12-02** |
| `journal.cdu.edu.cn/info/1058/1333.htm` | 2005-12-01 | **2015-12-01** |
| `journal.cdu.edu.cn/info/1058/1334.htm` | 2005-12-01 | **2015-10-15** |

全量重抓（`data/snapshot.json`，73 来源、18342 篇）对比修复前快照：

- **丢失日期 0 条**（没有把原本正确的日期改空）；
- 新增可识别日期 **1577 条**，日期被纠正 **54 条**（逐条核对页面原文，均为修复前有误）；
- **未来日期（>`lastUpdated`）从 2 条降到 0 条**；
- 标题尾部夹带日期的记录从 **490 条降到 67 条**，且剩余 67 条全部来自 2026-09-14 那次旧抓取、本次未重抓的页面；
- 本次新抓取的 16755 条里：夹带日期标题 **0 条**，无日期比例 **9.0%**（旧快照为 21.1%）。

**回归测试**（`internal/crawler/parser_test.go` 新增 7 个用例 / 10 个断言组）：

- `TestDetailPrefersPublicationDateOverHeadlineValidityPeriod`（三例：标题容器在前、标题与日期同容器、整页只有有效期）
- `TestListDateDoesNotLeakFromSiblingRows`、`TestRowSummaryEventDateIsNotUsedAsPublicationDate`
- `TestHeadlineEdgeDateDuplicateIsTrimmed`、`TestDateInsideArticleAnchorBecomesPublicationDate`
- `TestBareDateElementInsideAnchorIsPublicationDate`、`TestDeadlineOnlyRowKeepsNoDate`

`go test ./...`、`go test -race ./internal/...`、`go vet ./...`、`gofmt -l` 全部通过。

---

## 二、审查发现（按优先级，详见各报告）

### 建议尽快处理

1. **前端排序比较器每篇文章都新建 collator**（`frontend/src/App.tsx:295-300`）：全量排序实测 600–970 ms，
   改用共享 `Intl.Collator('zh-CN')` 后约 25 ms。叠加「同步中每 2 秒轮询换新数组 → 全量重算」，
   主线程会周期性冻结接近 1 秒（`backend.md` BUG-1 是同一现象的服务端一侧：写锁内 10 MB 全量序列化）。
2. **`dateInferred` 是死字段**：`parser.go` 构造 Article 时从未置位，快照 0 条为 true，但
   `App.tsx:389` 的「年份推断」徽标、CSV 的「年份推断」列、同步弹窗文案和 README 都在承诺这个能力。
   建议二选一：要么补齐后端推断并写入该字段，要么删掉前端三处与 README 的表述。
3. **`detailBudget = 12` 截断**（`crawler.go:27`）：20 个来源超出预算，约 3178 篇因此拿不到详情页日期；
   且预算只挑 `Date==""` 的条目，列表页已写错的日期永远得不到纠正。建议把预算提到可配置、
   并允许详情页结果覆盖列表值（`dates.md` 第 3 节有逐源证据）。
4. **`api.ts` 的 fetch 无超时**（`frontend/src/api.ts:38-51`）：后端挂起时 `refresh` 既不进 `catch`
   也不进 `finally`，页面会永远停在骨架屏；另外 `throw` 写在 `try` 里再 `catch` 区分，`error` 为对象时文案会变成 `[object Object]`。
5. **`internal/service` 写锁内做 O(n) 索引重建 + 全量排序 + 10 MB `MarshalIndent`**（`service.go:206-269`）：
   每个来源完成都持锁约 80–160 ms，73 个来源累计 6–12 s，期间 `GET /api/snapshot` 会被排队。

### 可以稍后处理

- `cmd/crawl` 的退出码恒为 0（成功/失败/取消不区分），Ctrl+C 后要等下一个 tick 才退出（`backend.md` BUG-9）。
- `/api/sync` 的所有业务校验错误一律返回 409（`server.go:34-37`），参数不合法应是 400。
- 移动端侧栏用 `translateX` 移出但未 `inert`/`aria-hidden`，键盘仍可 Tab 进不可见控件（`frontend.md` §3.6）。
- `fetch` 的 charset 探测只在响应头 + 前 1024 字节内生效，头部 meta 靠后或 CT 误标时会整页乱码（当前 cdu 各站未触发）。

### 明确不是 bug 的两类「异常日期」

- `xczgb.cdu.edu.cn/info/1036/1143.htm` 等史志页面，页面自己写 `发布日期：1938-01-01`（同系列 1922/1925/1927/2002），
  属于**源站标注**，保持原样。
- `dfl.cdu.edu.cn/info/1025/11546.htm` 写 `时间：2002-03-03 05:16`，是源站登记的招聘启事时间，也保持原样。

---

## 三、本次改动清单

```
M  internal/crawler/parser.go       日期取值重写（语义过滤 + 候选分级 + 兄弟行隔离 + 行内日期回收）
M  internal/crawler/parser_test.go  7 个日期回归用例
M  README.md                        日期口径补充；测试说明补充两类回归
?? research/review/                 四份审查报告 + 可重跑脚本 + 本总览
```

`data/snapshot.json` 已用修复后的解析器全量重抓（该目录在 `.gitignore` 中，不进版本库）。
