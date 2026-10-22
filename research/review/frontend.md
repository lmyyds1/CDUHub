# 前端审查报告：日期展示 / 筛选 / 搜索性能 / React 正确性

审查对象：`frontend/src/App.tsx`、`frontend/src/api.ts`、`frontend/src/Icon.tsx`、`frontend/src/main.tsx`、`frontend/vite.config.ts`、`frontend/embed.go`
审查方式：只读静态审查 + 用真实快照 `data/snapshot.json`（**16753 篇文章 / 73 个来源**）跑等价算法基准（Node 25 / V8，benchmark 脚本仅在系统临时目录，未写入仓库）。
构建验证：`frontend/node_modules` 存在；`npx tsc -b --force` **通过（exit 0）**；`npm run build` 在本沙箱内于 `vite.config.ts` 加载阶段失败（`esbuild` spawn EPERM，属沙箱限制而非代码错误），**bundle 未实际产出，`frontend/dist` 未被修改**。详见 §6。

标签约定：`确认 bug` = 可复现的错误行为或与实现/文档矛盾；`体验问题` = 行为正确但体验/性能差；`死代码` = 永远不执行的分支或永不存在的字段。

---

## 0. 结论摘要（按严重度）

| # | 严重度 | 标签 | 位置 | 一句话 |
|---|---|---|---|---|
| 1 | P0 | 确认 bug（性能） | `App.tsx:281-301` | 轮询每次 `setSnapshot` 换新数组 → `filteredArticles` 全量重算，实测单次 0.6–1.0 s，同步中每 2 s、空闲每 10 s 一次，主线程直接卡死 |
| 2 | P0 | 确认 bug（搜索质量） | `App.tsx:62-88` | `fuzzyMatch` 的子序列捷径把「只要字符按序出现」判为命中，`abcdef` 这类无意义查询命中 30 条；逐键 50–190 ms |
| 3 | P1 | 死代码 + 确认 bug（文案） | `App.tsx:109,389,407` + `parser.go:659` | `dateInferred` 后端恒为 `false`，「年份推断」徽标/CSV 列/“推断年份会单独提示”文案全部不可达 |
| 4 | P1 | 确认 bug（跨层） | `App.tsx:293,299,389` | 2 条「试用有效期 2026-12-31」被当成发布日期：置顶于「最新发布」，且在任何时间筛选下永远消失 |
| 5 | P2 | 确认 bug | `api.ts:42-49,38-51` | `throw` 写在 `try` 里再用 `catch` 区分，`error` 字段非字符串时消息变 `[object Object]`；且 fetch 无超时/AbortController，服务挂起则 `loading` 永真 |
| 6 | P2 | 体验问题（性能） | `App.tsx:319` | `todayCount` 未 memo，每次 render 全量扫 16753 条并调 16753 次 `localDate()`，实测 ≈7.4 ms/render |
| 7 | P3 | 体验问题（a11y） | `App.tsx:334-335` + `styles.css:6` | 移动端关闭的侧栏用 `transform` 移出屏幕但仍在 Tab 序内，键盘可聚焦到看不见的导航 |
| 8 | P3 | 体验问题 | `styles.css:7` + `App.tsx:389` | ≤480 px 时 `.article-date>span{display:none}` 把「年份推断」和「原始月日」提示全部隐藏，手机上反而看不到日期说明 |

---

## 1. 搜索 / 排序性能量级估算（任务重点）

### 1.1 数据结构事实（实测 `data/snapshot.json`）

| 指标 | 值 |
|---|---|
| 文章数 | 16 753（73 个来源、804 个「来源+栏目」组合、2444 个不同日期） |
| 可搜索文本 `articleSearchText` 长度 | 平均 **89.1** 字符，p50 **53**，p90 **106**，最大 **4 075** |
| 有 `content`（列表页摘要）的文章 | 5 186 条（其余为空串，故 69% 文章只有标题/来源/栏目/日期可搜） |
| 标题平均长度 | 27 字符 |
| 完全无日期 / 只有月日 | 3 528 条（21.06%）/ 其中 1 194 条为 `MM-DD` |
| `dateInferred == true` | **0** 条 |

### 1.2 单次按键的搜索成本（等价实现实测，含 `normalizeSearch` 重复调用）

| 模式 / 查询 | 耗时 | 每条文章 | 命中数 |
|---|---|---|---|
| contains `通知` | 25 ms | 1.51 µs | 3 196 |
| contains `关于2026年国庆节放假安排的通知` | 21 ms | 1.28 µs | 0 |
| fuzzy `通知` | 42 ms | 2.5 µs | 3 260 |
| fuzzy `奖助学金` | 107 ms | 6.4 µs | 24 |
| fuzzy `关于2026年国庆节放假安排的通知`（22 字） | **193 ms** | 11.5 µs | 6 |
| smart（默认模式）`奖助学金` | 58 ms | 3.5 µs | 24 |
| smart `关于2026年国庆节放假安排的通知` | **125 ms** | 7.5 µs | 6 |
| smart `教学科研项目申报的通知` | 92 ms | 5.5 µs | 8 |

**量级结论**

- 每字符成本：`normalizeSearch`+拼接 ≈ **6.6 ns/字符**（16.7k 条文本一遍 ≈ 18 ms：拼接 9.9 ms + 归一化 8.3 ms，每次查询都重建）。
- `fuzzyMatch` 编辑距离是 **O(len(text) × len(needle))**：典型文章 89 字符 × 8 字查询 ≈ 712 个 DP 单元/文章；22 字查询 ≈ 1958 单元/文章。全量一次 ≈ **1.2 万–3 300 万个单元**（实测 42–193 ms 与之吻合，≈6 ns/单元）。
- smart 模式对词做同义词展开（单组最多 4 个候选词，`App.tsx:46-60`），故成本 ≈ fuzzy ×（1+同义词数），且展开发生在**每条文章内层循环**（`App.tsx:101`），`expandTerm` 结果没有缓存，属重复计算。
- 额外 GC 压力：DP 在 `App.tsx:79-86` 每行都 `const current = [row]` 新建数组，16.7k 文章 × 平均 89 行 ≈ **每次查询 150 万个短数组**。

**判定：有明确卡顿风险。** 逐键 50–190 ms 已超过 100 ms 的可感知阈值；而更严重的是下面第 1.3 节的全量排序。

### 1.3 排序：真正的卡顿主因（实测）

`filteredArticles` 的排序（`App.tsx:295-300`）在**无筛选**时对 16 753 条全量排序，比较器里两次 `localeCompare`：

| 比较器写法 | 全量 16 753 条排序耗时 |
|---|---|
| 当前实现（`(a.date||'').localeCompare(...)` + `a.title.localeCompare(b.title,'zh-CN')`） | **0.60–0.97 s**（5 次：672/652/846/973/962 ms） |
| 标题改为共享 `Intl.Collator('zh-CN')` | **25 ms** |
| 标题比较去掉 locale 参数 | 18 ms |
| 只比较日期、无标题兜底 | 8 ms |
| 已过滤到 256 条（range=7 典型值） | 14.7 ms |

即：**同一份数据、同一语义，只把「每次比较都带 locale 参数」换成复用的 `Intl.Collator`，排序从 ~700 ms 降到 25 ms（≈28 倍）**。*说明：这是 Node/V8/ICU 下的实测值，浏览器/WebView2 的 ICU 实现可能量级不同，但「每次比较新建 collator」这条开销方向一致；修复建议本身与具体引擎无关。*

叠加第 1.4 节：**筛选/排序本身没被真正 memo 住**，于是这个 ~700 ms 排序会在轮询时反复出现。

### 1.4 轮询把「一次昂贵的重算」变成「每 2–10 秒一次」

- `App.tsx:186-195`：`poll()` → `refresh()` → `setSnapshot(data)`；`data` 是每次 `fetch('/api/snapshot')` 反序列化的**新对象/新数组**，因此 `filteredArticles` 的依赖 `snapshot.articles`（`App.tsx:301`）**每次轮询都变**。
- 同步中轮询间隔 **2 s**（`App.tsx:191`），空闲 **10 s**。实测单次重算（空查询、无筛选 = 纯 filter + 上述排序）≈ **650–940 ms**；快照 JSON 本身 7.8 MB、`JSON.parse` 17 ms 只是零头。
- 后果：同步期间页面基本处于「每 2 秒冻结近 1 秒」的状态；空闲期每 10 秒一次可感知掉帧。打字时则叠加第 1.2 节的 50–190 ms。
- 同类问题：`channels`（`App.tsx:279`）、`filteredSources`（`:302-311`）、`sourceOptions`（`:280`）也随 `snapshot` 引用变化而重算；`bookmarks` 被列进 `filteredArticles` 依赖（`:301`），而书签只在 `view === 'saved'` 时才影响结果（`:286`），导致**在信息流里点一次收藏也会触发一次全量重算 + ~700 ms 排序**。

### 1.5 建议（按性价比排序，均为最小改动）

1. **先治轮询**（收益最大、改动最小）：轮询结果与上一次比较，仅在真正变化时更新状态；或把 `filteredArticles` 的依赖换成稳定键（如 `snapshot.lastUpdated` + `snapshot.articles.length`），并用 `useRef` 持有 articles 进 memo 计算。
2. **排序改用共享 `Intl.Collator`**（`const collator = new Intl.Collator('zh-CN')` 模块级常量），或预计算 `key = date + '\u0000' + title` 后按字符串比较；合计可把 700 ms → ~20 ms。
3. **预索引**：`articleSearchText`/`normalizeSearch` 结果用 `useMemo` 或 `WeakMap<Article,string>` 缓存（按文章对象引用），每次查询省 ~18 ms；进一步可建单字倒排（当前 16.7k 条、p50 53 字符，索引体积可接受）。
4. **`useDeferredValue(query)`**：把搜索框的 `query` 用 `useDeferredValue` 延迟给过滤层，输入框保持即时响应（React 18 已具备）。
5. **严重卡顿场景再上 Web Worker**：把 filter+sort+search 移到 Worker，主线程只收 `visibleArticles`（15 条）。当前数据量下，做完 1+2 后 60–100 ms 的量级可接受，Worker 属可选加固。
6. 顺手优化 `fuzzyMatch`：复用两个 `Int16Array`（`previous`/`current`）替代每行新建数组；`expandTerm` 结果按 term 做 `Map` 缓存；contains 模式对 `normalizeSearch(text)` 只归一化一次（`App.tsx:99` 目前每个词归一化一次）。

---

## 2. 日期相关：死代码与体验问题（任务重点）

### 2.1 【死代码 + 确认 bug（文案）】`dateInferred` 永远为 false，「年份推断」整条链路不可达

- 证据链：`parser.go:659` 构造 `model.Article` 时**没有给 `DateInferred` 赋值**（结构体零值 false）；`parser_test.go:77` 断言 `items[1].DateInferred` 为 false；`service.go:234` 只在「新抓取无日期」时**继承旧值**，没有任何地方置 true；真实快照 16 753 条中 `dateInferred == true` 的数量为 **0**。
- 前端不可达/误导点：
  - `App.tsx:389` `{article.dateInferred ? <span title="...">年份推断</span> : ...}` —— **永不渲染**（连同 `title` 里的「年份根据采集时间推断」提示）。
  - `App.tsx:109` CSV 列「年份推断」永远输出「否」——对用户是无效列。
  - `App.tsx:407`（同步管理弹窗帮助文案）：**“日期缺失会明确标注，推断的年份会单独提示”** —— 与实现矛盾，属**确认 bug（文案）**；README:73 描述“不按当前年份猜测、保留原始月日”，前端实现的却是另一套说辞。
- 触发场景：任意一次同步、任意一篇文章、CSV 导出、打开「同步管理」弹窗。
- 影响：误导用户以为存在“推断年份”能力；CSV 多一列恒「否」；代码留下无法测试的分支。
- 建议：二选一——(a) 删掉 `App.tsx:389` 的推断分支、CSV 列、弹窗文案，`model.go:33`/`api.ts:11` 的 `dateInferred` 一并移除（推荐，与解析器真实行为一致）；(b) 若确实需要详情页补年份，则在 `parser.go` 里实现并置位，再补测试。注意 `service.go:234` 目前保留了该字段的继承逻辑，选 (a) 时应同步删除。

### 2.2 【确认 bug（跨层）+ 体验问题】未来日期 2026-12-31：置顶于「最新发布」，且在任何时间筛选下永远消失

- 数据事实：快照中日期晚于审查日（2026-10-21）的记录恰好 **2 条**，均为图书馆栏目「试用有效期」被目录页解析成发布日期：
  - `2026-12-31 / 2026年12月31日 / 资讯动态 / 图书馆 / 古扬“外文学术资源整合服务系统”（试用有效期：即日起--2026年12月31日）`
  - `2026-12-31 / 2026年12月31日 / 资讯动态 / 图书馆 / Mirror（米乐）心理资源服务平台（试用有效期：即日起--2026年12月31…`
- `App.tsx:299`（排序）：`newest` 时 `2026-12-31` > `2026-09-14`，这两条**固定排在信息流最顶部**，被当作“最新发布”，实际是“到期日”。
- `App.tsx:293`（range 过滤）：`articleDay(article) > localDate()` 会把它们从「今日 / 最近 7 天 / 30 天 / 90 天」**全部排除** —— 与“这两条其实是近期发布”的真实情况相反；同时 `App.tsx:388` 的「今日」徽标、`:319` 的今日计数也永远取不到它们。
- 触发场景：打开信息流（默认排序）→ 顶部两条日期为 2026-12-31；切「最近 7 天」→ 它们消失。
- 影响：新鲜度展示错误（置顶的“最新”是半年后的到期日）；同一批数据在两种视图下表现矛盾。
- 建议：前端加一层防御 `if (articleDay(article) > localDate()) return false`（或至少降权/标注「日期异常」），不要让它进 `newest` 顶部；根因在解析器（`parser.go` 日期候选应排除「有效期/截止/至」等上下文），建议转给后端负责同学，前端侧先做兜底。

### 2.3 【体验问题】无完整日期记录：`range` 过滤直接排除，与 README 一致，但展示文案重复/误导

- `App.tsx:293` `!article.date` → 直接 `return false`。README:73「缺少完整日期的记录会显示说明，**且不计入限定日期范围的结果**」——**行为与文档一致，不算 bug**。但需注意量级：**3 528 条（21.06%）**在任何时间筛选下都不可见；其中 1 224 条有 `dateText`（如 `08-28`、`2026/09/14`）。用户选「最近 7 天」时，这 1/5 的数据静默消失，界面没有任何说明。
- `App.tsx:389` 展示层：`<time>{articleDay(article) || '日期未标注'}</time>` + `{!article.date && article.dateText ? <span title={dateText}>{dateText}</span> : null}`，于是这类记录显示为「**日期未标注 08-28**」——既说未标注又给出月日，语义冲突且重复占位。
- 建议：`<time>` 文案改为「08-28（年份未标注）」或「日期不完整」；`range` 过滤生效且有被排除的无日期记录时，在 `list-header`/空态提示「另有 N 条日期不完整的信息未参与时间筛选」。README 也可以补一句“当前快照约 21% 记录日期不完整”。

### 2.4 【体验问题】日期筛选边界与 `localDate()` 的时区/性能细节

- `App.tsx:282-284`：`cutoff.setDate(getDate() - Number(range) + 1)`，即「最近 7 天」= 含今天共 7 个自然日（本地时区）；`localDate()`（`:34-36`）用 `getFullYear/getMonth/getDate` 取**本地**日期，而 `fetchedAt`/`lastUpdated` 是 RFC3339 带时区（`+08:00`）——两者在**同一台机器同一时区**下一致，但用户机器时区不是 UTC+8 时，「今日」会与学校所在时区错位（例如 UTC 用户在 08:00 前看到“昨天”）。这属可接受的取舍，建议在文案或 README 注明“按本机时区”。
- `App.tsx:293` 在 `Array.filter` **内部**每条都调用一次 `localDate()`（16 753 次构造 Date + 字符串拼接），实测 **7.37 ms/pass**；应为循环外常量。
- `App.tsx:281-301` 的 memo 依赖里没有“当前日期”这类会跨天变化的量：跨零点后「今日」过滤/徽标要等下一次轮询（≤10 s）才会纠正；`todayCount`（`:319`）因未 memo 反而不受影响。影响很小，记录备查。

### 2.5 【体验问题】`articleDay` 对非 ISO 日期无兜底

- `App.tsx:44` `article.date ? article.date.slice(0,10) : ''`。当前快照 `date` 全部是 `YYYY-MM-DD`（实测 0 条例外），所以暂无 bug；但若后端改成 RFC3339（`2026-09-14T00:00:00+08:00`）也能正确取前 10 位，而若出现 `2026/09/14` 这类格式则会得到 `2026/09/14`→按字符串比较仍可用但会破坏 `<time dateTime>` 的合法性。属于防御性建议，非当期 bug。

---

## 3. React 正确性

### 3.1 【确认 bug（性能）】`filteredArticles` 的 memo 实际未起到 memo 作用
见 §1.4。`App.tsx:301` 依赖 `snapshot.articles`（每轮询必变）+ `bookmarks`（仅 saved 视图需要）。建议：拆分「搜索/过滤」「排序」两个 memo；`bookmarks` 用 `Set` 并在 `view !== 'saved'` 时从依赖/逻辑中剔除；`filteredArticles` 依赖 `snapshot.lastUpdated` 而非数组引用。

### 3.2 【体验问题】`todayCount` 每次 render 全量扫描
`App.tsx:319` `snapshot.articles.filter(article => articleDay(article) === localDate()).length`：每次输入、每次 toast、每次分页都会跑 16 753 次 `localDate()`，实测 **≈7.4 ms/render**。用 `useMemo(..., [snapshot.articles])` 包一层即可（顺带把 `localDate()` 提到外面）。

### 3.3 【已核对无问题】轮询定时器链 `App.tsx:186-195`
`let alive = true` + `clearTimeout(timer)` + `if (alive) timer = setTimeout(...)` 正确防止了 StrictMode 双挂载产生两条轮询链（首次 `void poll()` 后清理时只有第一个 `setTimeout` 被记住——注意 `timer` 只保存最新一个，但因为链是串行的，任一时刻最多只有一个待执行定时器，所以 `clearTimeout` 足够）。`refresh` 内部 `try/catch/finally` 保证 `setLoading(false)` 一定执行。**唯一缺口**：`runningRef` 只在成功时更新（`:180`），服务挂掉后若之前是 `running: true`，会以 2 s 间隔无限重试并持续打日志——建议 catch 里也 `runningRef.current = false`（退化为 10 s）。

### 3.4 【确认 bug】`fetch` 无超时，`loading` 可能永真
`api.ts:38-51` 的 `fetch` 没有 `AbortController`/超时；`App.tsx:176-184` 的 `refresh` 在 `await getSnapshot()` 挂起时既不会进 `catch` 也不会进 `finally`，`loading` 永远为 `true` → 页面永远显示骨架屏（`App.tsx:368,382,386`），`connectionError` 也不会出现。触发场景：后端进程卡死/半开连接（不是拒绝连接）。建议：`AbortSignal.timeout(10000)`，超时后抛出并在 `catch` 里设置 `connectionError`。

### 3.5 【已核对无问题，含 1 条小建议】`Dialog` 焦点管理 `App.tsx:123-150`
- `useEffect(..., [])` + `closeRef.current = close` 正确规避了 stale closure；`previous?.focus()` 在卸载时恢复焦点；`document.body.style.overflow` 有旧值恢复；Tab 陷阱覆盖 `button/a/input/select/summary/[tabindex="0"]`，且 `disabled` 元素被排除；`Escape` 关闭。实现质量高于平均水平。
- 小建议：`aria-labelledby="dialog-title"`（`:145`）与 `<h2 id="dialog-title">`（`:146`）是固定 id，当前同一时刻只有一个弹窗，无冲突；若将来支持嵌套弹窗需改成 `useId()`。另外 Tab 陷阱未把 `document.activeElement` 落到弹窗外的情况收回（如用户用鼠标点到弹窗外再按 Tab），可加 `focusin` 校验，属加固项。

### 3.6 【体验问题】`localStorage` 读写
- `App.tsx:19-21` `readStorage` 用 `try/catch` 包裹 `JSON.parse`，损坏数据回落默认值 —— 正确。
- `App.tsx:230-231,262-263` `setItem` 有 `try/catch` 并给出 toast —— 正确。
- 【体验问题】`App.tsx:168` 收藏只有 id 列表，**不与快照校验**：同步后文章被移除时 `bookmarks.length`（`:343` 导航角标）仍计数，而 `view === 'saved'` 下列表为空 → 空态显示「把值得关注的信息收藏在这里 / 点击信息右侧的书签」（`:390`），与“我有收藏但都不在了”的事实矛盾。建议区分「暂无收藏」与「收藏的 N 条已不在当前快照中」。
- 【体验问题】`App.tsx:226-233` `toggleBookmark` 用闭包里的 `bookmarks` 计算 `next`，同一 tick 内连续点击两次会丢一次更新（React 18 批处理下 `setBookmarks(next)` 用值而非 updater）。当前 UI 一次只能点一个按钮，实际风险低；建议改成 `setBookmarks(current => ...)` 并用 `useEffect` 持久化。

### 3.7 【已核对无问题】CSV 导出 `App.tsx:103-121`
- 注入防护：`/^[\s]*[=+@\-]/` 命中则在最前加 `'`，随后整体加双引号并把 `"` 转义为 `""` —— 覆盖 `=`、`+`、`-`、`@` 及前置空白（含 Tab/CR，因为 `\s` 包含它们），**结论：有效**。
- BOM：`'\uFEFF' + ...`（`:112`）—— 有效，Excel 中文不乱码。
- `URL.revokeObjectURL` 延迟 1 s 回收、`anchor.remove()` —— 正确。
- 建议（体验）：导出的是 `filteredArticles` 全量（可达 16 753 行、数 MB），按钮文案只写「导出 CSV」不提示条数范围；可在成功 toast 里带上「（当前筛选结果）」或加“仅导出当前页”选项。

### 3.8 【体验问题】无 ErrorBoundary
`main.tsx:6-8` 直接把 `<App/>` 挂在 `React.StrictMode` 下。任意渲染期异常（例如后端返回 `date` 为非字符串导致 `.slice` 抛错）都会让整页白屏且无提示。建议加一个最小 `ErrorBoundary`，显示「界面出现异常，点击重载」。

---

## 4. `api.ts`

### 4.1 【确认 bug】`request` 中的“catch 内 throw”写法
```ts
if (!response.ok) {
  const message = await response.text()
  try { throw new Error(JSON.parse(message).error || `请求失败 (${response.status})`) }
  catch (error) {
    if (error instanceof SyntaxError) throw new Error(`服务请求失败 (${response.status})`)
    throw error
  }
}
```
- `api.ts:42-49`。逻辑上能工作（自己抛的 `Error` 被同层 `catch` 接住后原样再抛），但**可读性差且有两个真实缺陷**：
  1. 若响应体是合法 JSON 但 `error` 字段是对象/数组，`new Error({...})` 的 `message` 变成 `[object Object]`，用户看到「无法开始同步：[object Object]」；
  2. 若 `error` 字段为空字符串/null，`||` 回落到 `请求失败 (status)` —— 行为可接受；但服务端返回纯文本（如反向代理 502 HTML）时会被判为 `SyntaxError` 分支，丢失服务端文本，只显示状态码。
- 建议改为不使用 `throw` 做控制流：
  ```ts
  let detail = ''
  try { const body = JSON.parse(message); if (typeof body?.error === 'string') detail = body.error } catch { detail = message?.slice(0, 200) || '' }
  throw new Error(detail || `请求失败 (${response.status})`)
  ```
  这样文本/对象/缺失三种情况都有合理消息，且不再有“catch 吞掉自己抛的错”。

### 4.2 【已核对无问题】Wails / HTTP 双适配
- `api.ts:54-55,65-66,69-71,78-79`：统一 `window.go?.main?.App` 存在性判断，bridge 路径与 `fetch` 路径返回值形状统一（`startSync` 对 `void` 与 `{ok}` 都做了 `!result.ok` 保护，`:67`）。`App.d.ts` 显示 `StartSync` 返回 `Promise<void>`，与 `api.ts:26` 的联合类型一致。
- `getSnapshot`（`:53-62`）用 `{...emptySnapshot, ...data}` 兜底缺字段，`sources[].channels`、`sync.errors` 都做了 `[]` 归一 —— 稳健。
- 小建议：`getSnapshot` 里若 `data` 为 `null`（bridge 异常返回），`data.sources` 会抛 TypeError；改用 `const safe = data || emptySnapshot` 更稳（当前会被 `refresh` 的 catch 接住，只是错误文案变成 "Cannot read properties of null"）。

### 4.3 【体验问题】`isSafeURL` / `openURL`
- `api.ts:73-75`：只允许 `http:`/`https:`，挡掉 `javascript:`/`data:` —— **方向正确**。可补两点：(a) 拒绝带 `username:password@` 的 URL（`new URL('https://a:b@evil.com').protocol === 'https:'` 会通过，钓鱼观感差）；(b) 部署在 https 时允许 `http:` 会产生混合内容/被浏览器拦截，可提示。
- `api.ts:76-81` 的 `else window.open(...)`（HTTP 模式兜底）实为**近似死代码**：唯一调用方 `App.tsx:265-271` 在 `window.go?.main?.App` 不存在时**不**调用 `openURL`，而是放行 `<a target="_blank">` 默认行为。可保留，但建议要么删掉该分支，要么让 HTTP 模式也统一走 `openURL`（便于埋点/统一 `noopener`）。
- 【体验问题】`App.tsx:272` 的 `outbound`：URL 不安全时 `href: undefined`，`<a>` 无 `href` **不可聚焦、不被读屏识别为链接**，只能靠鼠标点击触发 toast（`:266`）。建议不安全时渲染 `<button>` 或 `role="link" tabIndex={0}` + `aria-disabled`。

---

## 5. 交互与可访问性

1. 【体验问题】移动端关闭的侧栏仍可被 Tab 聚焦 —— `App.tsx:334-335` 的 `aside.sidebar` 始终渲染，`styles.css:6` 在 `max-width:760px` 用 `transform:translateX(-100%)` 移出视口但未设 `inert`/`aria-hidden`/`visibility:hidden`。触发：手机/窄窗键盘或读屏用户按 Tab 会依次进入 7 个看不见的导航项与「同步管理」「关于信息集」。
   建议：`mobileNav` 为 false 且 `matchMedia('(max-width:760px)')` 时给 `<aside>` 加 `inert`（或 `aria-hidden="true"` + `tabIndex={-1}`），并在打开时把焦点移入、关闭时还原（对齐 `Dialog` 的实现质量）。
2. 【体验问题】≤480 px 时原始日期提示被隐藏 —— `styles.css:7` 含 `.article-date>span{display:none}`，而「年份推断」徽标与「原始月日（如 08-28）」都用 `.article-date > span` 渲染（`App.tsx:389`）。手机端用户看不到日期说明，只剩“日期未标注”。建议改为只隐藏低优先级信息，或在窄屏用 title/展开行承载。
3. 【体验问题】分页边界 —— `App.tsx:312-314`：`currentPage = Math.min(page, pageCount)` 与按钮 `disabled={currentPage <= 1}` / `>= pageCount` 配合正确，页码显示用 `currentPage` 而非 `page`，无越界。但 `page` 状态本身不会随 `pageCount` 缩小而下调（只有筛选变化时的 `useEffect`（`:208`）才重置为 1）：若快照更新导致结果变少，用户从第 5 页回到第 1 页后若数据再变多，会“跳回”第 5 页。建议在 `pageCount < page` 时 `setPage(pageCount)`。
4. 【已核对无问题】排序稳定性 —— `App.tsx:295-300` 无日期项统一 `return 1/-1` 置底，同日期按标题 `localeCompare('zh-CN')` 兜底，`Array.prototype.sort` 在现代引擎稳定；`date` 全部为 ISO 字符串，字符串比较等价于时间比较。唯一“体验”点是同标题同日期时依赖引擎稳定性（当前无重复 `id`，实测 0 条重复），可接受。
5. 【体验问题】控件 disabled 条件 —— `App.tsx:358` 同步按钮 `disabled={busy || loading || !!connectionError}`；`:382` 单站同步按钮额外要求 `isSafeURL(source.url)`；`:390` 空态按钮 `disabled={busy || snapshot.sync.running || !!connectionError}`；`:407` 弹窗内"停止同步"只判 `busy`。整体一致。小问题：`connectionError` 状态下所有启动类按钮被禁用，但「同步管理」弹窗里的「保存采集设置」不受影响（合理）；另外 `App.tsx:203` 的 Ctrl/⌘+K 未判断 `event.key` 大小写（CapsLock/Shift 下为 `K` 则失效），且未排除 `event.target` 为输入框（在搜索框内按无副作用，可忽略）。
6. 【体验问题】空态文案分支 —— `App.tsx:390` 的三元链对 `view === 'saved' && hasFilters` 的情况落入「没有找到匹配的信息」（合理）；但 `saved` 视图无收藏且快照尚未同步成功时，文案仍是「点击信息右侧的书签」。建议按 `snapshot.articles.length === 0` 再细分一层。
7. 【已核对无问题】toast —— `App.tsx:196-200` 用 `useEffect` + `clearTimeout` 管理 4.8 s 自动消失，新的 toast 会重置计时器；`:409` `role="status"` + 手动关闭按钮。可加 `aria-live="polite"`（`role=status` 已隐含 polite，够用）。
8. 【体验问题】来源筛选浮层 —— `App.tsx:377` 用 `<details>` 实现，勾选 checkbox 不关闭、点外部不关闭、无 Esc 关闭；`source-filter-panel` 在窄屏是 `position:fixed` 的底部抽屉（`styles.css:11,14`），无遮罩与焦点管理。可用但不如 `Dialog` 严谨，建议复用 `Dialog` 的焦点/Esc 逻辑或至少加点击外部关闭。
9. 【体验问题】`aria-current` 用在 `<button>` 上（`App.tsx:327`），规范上 `aria-current` 可用于任意元素，配合 `role` 更佳；`aria-pressed`（`:331`）用法正确。`#main-content` 存在但**没有 skip link**（`App.tsx:357`），键盘用户每次都要 Tab 穿过侧栏 8 个控件。建议加「跳到主要内容」链接。
10. 【死代码（无害）】`App.tsx:15` `statusLabels` 的 `ok` 与 `:277` `successSources`/`:278` `failedSources` 里的 `'ok'` 判断：后端当前只会产出 `pending/success/empty/partial/error/unavailable`（`crawler.go:173,193,317,319,324`、`parser.go:179`、`service.go:210,216`），`ok` 是历史遗留。另 `App.tsx:45` 的 `SearchableArticle` 与 `:91-94` 引用的 `body/summary/excerpt/description` 后端模型中不存在（`model.go:22-35`），属预留字段的死代码。两者都不影响功能，建议清理以免误读。

---

## 6. 构建与工程

1. 【已核对】`frontend/node_modules` **已安装**（`vite 6`、`react 18`、`typescript 5.7` 等齐全），`npx tsc -b --force` **exit 0**，说明类型层面无错误，也验证了 `App.tsx`/`api.ts` 的类型自洽（含 `IconName` 联合类型、`window.go` 声明）。
2. 【沙箱限制】`npm run build` 在 `vite.config.ts` 加载阶段失败：`Error: spawn EPERM`（`esbuild` 启动子进程被沙箱拦截）。这是本次审查环境的限制，**不是代码缺陷**；`frontend/dist` 未被写入（仍为构建前的 `index.html`/`assets/*`/`.gitkeep`）。因此**bundle 体积与产物未验证**，仅完成类型检查。
3. 【体验问题（构建顺序脆弱）】`frontend/embed.go:7` `//go:embed all:dist`：`.gitignore` 只提交 `frontend/dist/.gitkeep`（`.gitignore:21-22`），而 `vite.config.ts:11` `emptyOutDir: true` 会在每次 build 前**清空 dist（包括 .gitkeep）**。因此新克隆仓库直接 `go build ./...` 会因 `dist` 缺少被 embed 的文件而失败，必须严格遵循 README 的「先 `npm run build` 再 `go build`」；`emptyOutDir` 还会让 `.gitkeep` 显示为已删除的脏工作区。
   建议：改用 `vite.config.ts` 的 `build.emptyOutDir` + 保留文件（如 `public/.gitkeep`），或把 `//go:embed` 目标改为 `all:dist/*` 并在 `main.go` 侧对空目录给出清晰报错；至少在校验脚本里显式检查 `dist/index.html` 是否存在。
4. 【已核对】`vite.config.ts:5-9` 的 dev proxy（`/api` → `127.0.0.1:8080`，未开 `changeOrigin`）与后端 `sameOrigin`（`server.go:63-76`）兼容：Vite 转发时保留原 `Host`，`Origin` 与 `r.Host` 同为 `127.0.0.1:5173`，跨站校验通过，开发模式无 403。
5. 【小建议】`index.html` 未设置 CSP/`referrer` meta；HTTP 服务端已有 `X-Content-Type-Options`/`Referrer-Policy`（`server.go:52-54`），但 Wails 桌面端加载同一份 HTML 时没有这些头。当前无内联脚本注入面，风险低，可在 `index.html` 加一条保守 CSP 作为纵深防御。

---

## 7. 建议的修复顺序（给 lead 的落地建议）

1. **（半天内可完成，收益最大）** 排序改共享 `Intl.Collator`；轮询结果做变更比较或改用稳定依赖键；`todayCount` 加 `useMemo`；`localDate()` 移出 filter 循环。→ 消除「每 2–10 秒冻结近 1 秒」。
2. **（一天）** 搜索：contains 归一化复用、`fuzzyMatch` 复用 TypedArray、`expandTerm` 缓存；`query` 走 `useDeferredValue`；修 `fuzzyMatch` 子序列捷径的误判（建议要求子序列命中时词的覆盖密度 ≥ 阈值，或对 ≥3 字词直接跳过子序列捷径）。
3. **（半天）** 清理 `dateInferred` 死链路（前端 3 处 + 弹窗文案），或补齐后端置位逻辑；前端加未来日期兜底。
4. **（一天）** `api.ts` 错误解析改写 + fetch 超时；移动端侧栏 `inert` + skip link；窄屏日期提示恢复。

---

### 附：审查中未发现问题、确认健壮的部分（避免重复审查）

- `Icon.tsx`：23 个图标的 `viewBox/stroke` 一致，`aria-hidden="true"` 正确，未暴露无 `title` 的可访问名问题（图标均为装饰性，标签由外层按钮提供）。
- `main.tsx`：入口最小化，`StrictMode` 已启用。
- 收藏/设置的 `localStorage` 读写均有异常保护；设置项有 1–50 / 1–100 的钳制（`App.tsx:30-31,239-240,258-259`）。
- CSV 注入防护与 BOM 均正确实现（详见 §3.7）。
- 分页、排序稳定性、无日期项置底逻辑正确（详见 §5.3–5.4）。
- `Dialog` 的焦点陷阱、Esc、滚动锁、焦点还原实现正确（详见 §3.5）。
- Wails/HTTP 双适配的返回值归一与缺字段兜底正确（详见 §4.2）。
