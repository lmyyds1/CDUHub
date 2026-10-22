# 后端审查报告：service / server / cmd / Wails 入口

审查对象：`internal/service/service.go`、`internal/service/service_test.go`、`internal/server/server.go`、`internal/server/server_test.go`、`cmd/server/main.go`、`cmd/crawl/main.go`、`main.go`、`app.go`，并对 `internal/model`、`internal/crawler`、`frontend/src` 的相关契约做交叉核对。

范围约定：本地单机运行，安全/鉴权加固不在范围内；重点为并发正确性、数据一致性、故障可恢复性。
只读审查：未修改任何源文件、未写入 `data/`。

阅读约定：**确认 bug** 用 `BUG-n`、**设计取舍** 用 `D-n`、**测试缺口** 用 `GAP-n`；每条均给 file:line、触发条件、影响、建议。`BUG-n` 是稳定编号，不表示行文顺序，索引如下。

| 编号 | 结论 | 严重度 | 位置 |
| --- | --- | --- | --- |
| BUG-1 | 写锁内 O(n) 索引重建 + 全量排序 + 10 MB MarshalIndent 落盘，阻塞 Snapshot | 高 | service.go:206-269、285-301 |
| BUG-2 | `run` 无 panic 恢复；panic 前 defer 已把 phase 写成 completed 并落盘 | 中高 | service.go:133-151 |
| BUG-3 | `Close()` 无等待上限、无 closed 状态；Wails 退出可能挂起 | 中 | service.go:121-131，app.go:19 |
| BUG-4 | `cmd/server` Ctrl+C 不取消抓取，退出被 shutdown 窗口拖住 | 低 | cmd/server/main.go:35-68 |
| BUG-5 | 快照损坏时 Wails 桌面端静默退出，无提示/无恢复入口 | 中 | main.go:24-27 |
| BUG-6 | `DateInferred` 死字段，UI/CSV/文案却在承诺“年份推断” | 中低 | parser.go:659、model.go:33、App.tsx:108/389 |
| BUG-7 | 跨同步时栏目/标题/摘要可能被“降级覆盖” | 低 | service.go:236-239 |
| BUG-8 | `Start` 的所有业务校验错误一律返回 409 | 中低 | server.go:34-37 |
| BUG-9 | `cmd/crawl` Ctrl+C 不立即退出；成功/失败/取消一律退出码 0 | 中 | cmd/crawl/main.go:26-46 |
| BUG-10 | 仓库内临时诊断程序导致 README 的 `go test ./...` 失败 | 低（卫生） | research/tmpdiag、research/tmpreview-crawler |

## 0. 验证方法与实测证据

| 项目 | 命令 | 结果 |
| --- | --- | --- |
| 编译 | `$env:GOCACHE="$PWD\.cache\go-build"; go build ./internal/... ./cmd/... .` | 通过 |
| 静态检查 | `go vet ./internal/... ./cmd/... .` | 无输出（通过） |
| 单测 | `go test ./internal/...` | crawler / server / service 全部 ok |
| 竞态 | `go test -race ./internal/...` | 全部 ok（3.0–3.8s） |

刻意未运行 `go test ./...` / `go vet ./...`：`research/tmpdiag/`、`research/tmpreview-crawler/` 下有未跟踪的临时诊断程序（多个 `package main`、重复声明 `fetch`/`main`），会让整仓构建失败（见 BUG-10）。

快照实测（`data/snapshot.json`，2026-09-14 中断态）：

- 16753 条文章 / 73 个来源，文件 **9.67 MB**；`sync.running=true`、`phase=crawling`、`completed=60/73`（磁盘上留有中断态，正好可用于验证 §C 的“启动时改为 interrupted”恢复路径）。
- 来源状态：success 70、unavailable 2、partial 1；`sync.errors` 3 条。
- 文章：`date==""` 3528 条，其中有 `dateText`（月日部分日期）1224 条；`dateInferred==true` **0 条**；`content` 非空仅 5186 条（31%）。
- 体积构成：紧凑 JSON 8.00 MB，`MarshalIndent` 后 9.67 MB（+21%）；`content` 合计 1.67 MB；单条文章平均约 3.0 KB（受缩进影响）。

耗时量级估算（**方法透明**：受只读约束未在仓库内写基准程序；用 Python `json.dumps(indent=2)` 对同一份 9.67 MB 数据测得 0.145 s、索引重建 0.0020 s、原生排序 0.0012 s 作为下界代理，Go `encoding/json` 按常见 80–150 MB/s 反射吞吐推算）：

| 锁内步骤 | file:line | 估算耗时 |
| --- | --- | --- |
| 重建全量 `index map`（16.7k 条） | service.go:224-227 | 1–3 ms |
| 合并本来源文章 | service.go:228-244 | <1 ms |
| 全量重算 `ArticleCount`（O(n) 扫描） | service.go:245-250 | 0.2–0.5 ms |
| 全量 `sort.SliceStable`（16.7k，闭包比较） | service.go:257-263 | 3–8 ms |
| `json.MarshalIndent` 9.67 MB | service.go:292 | 60–120 ms |
| `os.WriteFile` 10 MB + `Rename` | service.go:297-300 | 10–30 ms |
| **合计（每个来源完成一次，持写锁）** | 206–269 | **≈ 80–160 ms** |

73 个来源 × 4 worker 串行争用同一把锁 ⇒ 一次全量同步的**累计持锁时间约 6–12 s**；单次 `GET /api/snapshot` 的排队上界 ≈（已在排队的 writer 数 + 1）× 单次持锁 ≈ **0.3–0.8 s**（Go `sync.RWMutex` 写优先，后到的 reader 会被排队中的 writer 挡住）。前端在同步中每 2 s 轮询一次快照（`frontend/src/App.tsx:189-191`），Wails 走 `GetSnapshot` 桥、Web 走 `/api/snapshot`，两条路径都受影响。

---

## A. 并发与锁粒度

### BUG-1（确认 bug，严重度：高）4 个 worker 在写锁内做 O(n) 索引重建 + 全量排序 + 10 MB 序列化落盘

- 位置：`internal/service/service.go:206-269`（锁区间），关键步骤 `224-227`、`245-250`、`257-263`、`266`；`saveLocked` 见 `service.go:285-301`。
- 触发条件：任意一次同步（`/api/sync`、Wails `StartSync`、`cmd/crawl`、`cmd/server -sync`），每完成一个来源触发一次；数据量越大越明显（当前 16753 条 / 9.67 MB）。
- 影响：
  1. **读侧延迟尖峰**：`Snapshot()`（service.go:60-73）虽只持 RLock 并做 3.3 MB 深拷贝（≈2–5 ms），但写优先语义下读者会被排在多个待写 worker 之后，同步期间快照接口出现 0.3–0.8 s 级卡顿；Wails 桌面端表现为界面轮询“发木”、进度条跳变。
  2. **写放大**：每次全量重写 9.67 MB，一次全量同步写盘 ≈ 73 × 9.7 MB ≈ **700 MB**；每天 2 次则 ~1.4 GB/天，对 SSD 寿命与机械盘 IO 都不友好。
  3. **CPU 浪费**：`MarshalIndent` 比紧凑编码多 21% 体积与相应 CPU（实测 8.00 MB → 9.67 MB），却只为人眼可读。
  4. **取消响应变钝**：`Close`/`Cancel` 需要等当前持锁者走完整套 80–160 ms 工作才能拿锁（见 BUG-3）。
- 说明：这**不是数据竞争**，`go test -race` 不可能暴露（实测通过）；它是锁粒度与复杂度问题，现有测试全部用 fake collector 且状态极小，也无法暴露（见 GAP-1）。
- 建议（按收益排序）：
  1. **常驻增量索引**：`Service` 增加 `index map[string]int`，只在 append/替换时维护，删除 `224-227` 的全量重建；`ArticleCount` 改为按 `SourceID` 维护计数器，删除 `245-250` 的 O(n) 扫描。
  2. **排序降级为“插入有序 + 归并”**：对同一来源的新文章先本地排序再与既有有序切片归并（O(n)），或在查询端排序；彻底移除锁内 `sort.SliceStable`。
  3. **保存去抖 + 锁外序列化**：锁内只做 `dirty` 标记与一次浅拷贝/快照，交由单一后台保存 goroutine 做 `MarshalIndent`/写盘（用递增 `saveSeq` 丢弃过期保存，`saveLocked` 语义改为 `saveSnapshot(state)`）；退出时在 `run` 的 defer 里强制同步落盘一次（保证崩溃可恢复语义不退化）。
  4. 磁盘格式改紧凑编码（或 gzip），`/api/snapshot` 的响应已是紧凑编码，无需改。
  5. 中期可考虑 SQLite/逐来源分文件，彻底消除“整库重写”模式。

### BUG-2（确认 bug，严重度：中高）`run` 没有 panic 恢复；且 panic 前 defer 会先把 phase 写成 completed 并落盘

- 位置：`internal/service/service.go:133-151`（`defer close(done)` + 匿名 defer），panic 源头可达 `s.collector.CrawlSource`（service.go:202）与 `Discover`（153）。
- 触发条件：任何来源的采集/解析代码 panic（HTML 结构极端、nil 解引用、未来改动引入的越界等）。
- 影响：
  1. goroutine 内的 panic **不会**被 `net/http` 的 per-request recover 接住，直接终止整个进程：Wails 桌面端 = 应用瞬间消失；`cmd/server` = 服务退出；`cmd/crawl` = 中途退出。
  2. 匿名 defer 先执行：把 `Running=false`、`Phase="completed"`（或 `cancelled`）**写盘**，随后 panic 才传播。于是磁盘状态谎称“本轮正常完成”，下次启动既不显示 interrupted，也不会提示失败，用户完全无从察觉（`NewWithCollector` 的中断检测 service.go:41-44 失效）。
- 建议：在 `run` 顶部 `defer func(){ if r := recover(); r != nil { 锁内 Phase="error"、Errors 追加 "采集程序异常: %v"、Running=false、保存 } }()`，与现有收尾 defer 合并；为 `Collector` 调用点再包一层 recover 便于定位来源。同时给 HTTP handler 增加一层 recover 中间件（返回统一 JSON 500），虽然 `net/http` 本身不会因此挂掉，但可保持错误响应契约一致。

### BUG-3（确认 bug，严重度：中）`Close()` 无超时上限、无 closed 标志；Wails 退出路径可能挂起

- 位置：`internal/service/service.go:121-131`（`<-done` 无超时），调用点 `app.go:19`（`OnShutdown` 同步调用）、`cmd/server/main.go:30`（defer）。
- 触发条件：
  - 采集器不尊重 ctx 的阻塞点：`crawler.fetch` 里 `charset.NewReader`/`html.Parse`（crawler.go:123-130）与 4 MiB 读取不可取消，但上界有限；真正无上界的是**未来**任何忽略 ctx 的实现、或 `saveLocked` 卡在杀毒软件/网络盘/磁盘满重试上。
  - Windows 关窗时 Wails 在 GUI 线程执行 `OnShutdown`，`Close()` 阻塞期间进程无法退出。
- 影响：关窗/退出“卡住不响应”，用户强杀进程；强杀时可能正处于 `WriteFile` 与 `Rename` 之间，留 `.tmp`（见 D-9）。此外 `Service` 没有 `closed` 标记，`Close()` 之后仍可 `Start()` 并重新拉起 goroutine（Wails 场景下未触发，但 API 语义不完整）。
- 建议：`Close(ctx)` 或内部固定上限 `select { case <-done: case <-time.After(5*time.Second): }`，超时后记录并放弃等待（进程退出由 OS 收尾，数据一致由 `Rename` 保证）；增加 `closed bool`，`Start` 在 closed 时返回 `errors.New("服务已关闭")`；`run` 的收尾 defer 应在上限内完成最后一次保存（保存本身有 80–160 ms 量级上界）。

补充判断（**非 bug，已核实**）：`run` 的两个 defer 顺序是先收尾（`135-151`）后 `close(done)`（`134`），因此 `Running=false` 早于 `done` 关闭存在一个毫秒级窗口；此时 `Start` 可成功并替换 `s.done`，旧 `run` 只再执行一次 `close(旧 done)`，不再触碰 `state`，故不会产生竞态或丢失 `close`；唯一副作用是该瞬间的 `Close()` 会改为等待新启动的 run。风险可忽略，但建议改为“收尾完成后由 run 自己置 `Running=false` 并 `close(done)`，再原子地清空 `s.done`”以消除这一窗口。

---

## B. Start / Cancel / Close 生命周期与定时器叠加

### BUG-4（确认 bug，严重度：低）`cmd/server` 收到 Ctrl+C 时不取消正在进行的抓取，退出被 5 s 的 shutdown 窗口拖住

- 位置：`cmd/server/main.go:35-36`（`signal.NotifyContext`）、`42-57`（interval 定时器）、`59-64`（`srv.Shutdown`）、`66-68` + `30`（`defer svc.Close()`）。
- 触发条件：`-sync` 或 `-interval` 启动了同步时按 Ctrl+C。
- 影响：信号只终止 ticker 与触发 HTTP `Shutdown`（≤5 s），**抓取 ctx 直到 `ListenAndServe` 返回、`defer svc.Close()` 执行才被 cancel**。因此按 Ctrl+C 后后台仍在继续请求官网（最长约 5 s，若 shutdown 有挂起连接更久），然后才进入取消收尾；用户观感为“按了没反应，然后才退出”。若同时有前端 `/api/snapshot` 长轮询卡在锁上，`Shutdown` 的 5 s 上限也被吃掉，但代码丢弃了返回值 `_ =`（main.go:63），无法观测超时。
- 建议：`go func(){ <-ctx.Done(); svc.Cancel(); ... }()` 让取消与 `Shutdown` 并行；`Shutdown` 的超时结果 `log.Print`；把 `-interval` 的 ticker 与手动同步的重叠判定从“仅靠 `state.Sync.Running`”升级为显式调度视图（如记录 `lastAutoStart`，避免取消后立刻被 ticker 重新拉起，见 D-3）。

### 关于“Start/Cancel/Close 竞态”的判定（**结论：串行化是充分的，无需数据竞争修复**）

- `Start`（service.go:75-111）在 `s.mu.Lock()` 内检查 `state.Sync.Running`、校验 `SourceID`、写 `s.cancel`/`s.done` 并起 goroutine；`Cancel`（113-119）与 `Close`（121-131）分别以 RLock 读取 `cancel`/`done`；`run` 全部 `state` 访问（153-171、176-188、199-201、206-269、135-151）都在 `s.mu` 内。`-race` 实测干净。
- `Cancel` 之后立刻 `Start` 会被 `state.Sync.Running==true` 拒绝，直到旧 run 的收尾 defer 把它置 false —— 语义正确（只有 409，不会双跑）。这也意味着 UI 上“停止同步 → 立刻开始同步”会失败一次（前端以 `busy` 稍作缓解，`App.tsx:249-251`）；属可接受的串行化，但建议在 UI 侧对 409 做“正在停止，请稍候”的专门提示。
- 旧 goroutine 在 Cancel 后继续写 `state` 的风险：只有 `run` 主 goroutine 与 4 个 worker 会写，且写点都在锁内；worker 在 `ctx.Err()` 命中后丢弃 in-flight 结果（service.go:203-205）。**不会出现“旧 run 覆盖新 run 状态”**，因为新 run 只有在旧 run 置 `Running=false` 后才能启动，而旧 run 之后不再写 `state`。
- interval 定时器与手动同步：`cmd/server/main.go:51` 调 `svc.Start`，`Start` 的锁内判重足以防止重叠；失败仅 `log.Print`（52）。真正的缺口是**取消语义**：定时器不知道“用户刚刚手动取消”，下个 tick 会照常启动新一轮（D-3），以及 BUG-4 的退出延迟。

---

## C. 持久化 `saveLocked`

### 判定项（**非 bug**）：空路径 `MkdirAll(filepath.Dir(""))` 不成立 —— 已核实

- `saveLocked` 第一段 `service.go:286-288`：`s.path == ""` 时直接 `return nil`（测试用内存态，`service_test.go:220`、`server_test.go:32` 就依赖它）。所以永远不会执行 `MkdirAll("")`。
- 非空且无目录的路径（如 `-data snapshot.json`）：`filepath.Dir("snapshot.json") == "."`，`os.MkdirAll(".")` 返回 nil，无害。
- `NewWithCollector` 对 `path==""` 的读取错误经 `errors.Is(err, os.ErrNotExist)` 判定（service.go:45）：Windows 上 `ERROR_PATH_NOT_FOUND` 映射到 `ErrNotExist`，因此空路径被当作“无缓存”而非致命错误（测试已固化该行为）。**结论：不是 bug，可放心。**

### 判定项：`.tmp + Rename` 的原子性、并发与中断

- 调用点只有两处且都在 `s.mu` 写锁内（worker service.go:266、收尾 147），**单进程内不会并发 `Rename`**；`os.Rename` 在 Windows 走 `MoveFileEx(..., MOVEFILE_REPLACE_EXISTING)`，对目标文件的替换是原子的（要么旧文件、要么新文件，不会截断），因此并行 `GET /api/snapshot` 不会读到半个 JSON。
- **缺 fsync**：`WriteFile` 后直接 `Rename`，没有 `f.Sync()`，也没有目录项持久化。断电/蓝屏可能丢失最后一次保存，甚至留下 0 字节文件（Windows 上概率低但存在）。
- **`.tmp` 残留**：若进程在 `WriteFile` 与 `Rename` 之间被杀，会留 `snapshot.json.tmp`；下次保存直接覆盖，无清理逻辑。无数据损坏风险，但会在 `data/` 留下 10 MB 垃圾。
- **0600 权限**：`os.WriteFile(tmp, data, 0600)` 在 Windows 上只影响“只读”属性，实际 ACL 继承父目录，其他本机用户可读；原来文件更宽的 ACL 不会被收紧。本地单机场景可接受。
- **损坏文件保留**：`NewWithCollector` 反序列化失败返回错误（service.go:37-40），不覆盖原文件（测试 `service_test.go:311-321` 覆盖），符合 README:120 承诺。但桌面端的处理见 BUG-5。
- 建议（合并为一条**设计取舍 D-6/D-9**）：写入后 `f.Sync()` 再 `Rename`；启动时顺手删除 `path+".tmp"`；磁盘格式可考虑“双文件轮换（snapshot.json / snapshot.bak）”以抵御半损坏。

### BUG-5（确认 bug，严重度：中）快照损坏时 Wails 桌面端静默退出，无 UI、无提示、无恢复入口

- 位置：`main.go:24-27`（`service.New` 错误直接 `log.Fatal`），`service.go:36-40`。
- 触发条件：`%APPDATA%\CDUHub\snapshot.json` 损坏（手工编辑、断电、磁盘错误、历史版本格式不兼容）。
- 影响：桌面程序**没有任何控制台**，用户双击后进程立即退出，看不到原因；也没有“重置数据库”的入口。数据本身被保留（好），但可用性为 0。对 `cmd/server` 而言 `log.Fatal` 尚可接受（终端可见）。
- 建议：桌面端把损坏文件重命名为 `snapshot.corrupt-<时间戳>.json`，以空库启动并在 UI 顶部提示“本地缓存损坏，已备份为 X，请重新同步”；或在 Wails 启动前用对话框（`runtime.MessageDialog`）呈现错误后退出。两种做法都不违反“不静默覆盖”的承诺。

---

## D. 合并语义

### 判定：Discover 只更新 Name/URL/Kind/Group（service.go:160-170）——**正确，不丢数据**

- 更新已有来源时只覆盖目录权威字段，保留 `Channels`/`Status`/`LastCrawled`/`Error`/`ArticleCount`，避免“一次目录故障清空栏目与状态”；新来源以目录值（`Status:"pending"`、`Channels:[]`）追加。测试 `service_test.go:107-109` 覆盖了“栏目瞬时缺失不擦除”。
- 但有两个**设计取舍**需登记：
  - **D-1（设计取舍）来源 ID 含 URL**：`parser.go:177` 用 `stableID(kind, name, raw)`，目录换了网址就生成新 ID，旧条目永久保留（README:76 明确“不因单次故障删除”）。后果：同一单位在 UI 出现两条（旧条 `unavailable`/旧 URL、新条正常），`ArticleCount` 分散。建议加 `AliasOf`/`superseded` 标记，或在 Discover 时按 `kind+name` 做“地址迁移”（保留旧 Articles 的 SourceID，仅更新 URL），至少在来源列表折叠旧条目。
  - **D-2（设计取舍）`href="#"` 来源**：目录修复后仍留 `unavailable` 旧条目（同 D-1 根因），UI 显示“暂无公开站点”但实际已有站点。可接受，但建议同 D-1 一并处理。

### 判定：`CrawlSource` 返回 err 时的 `updated.Status`（service.go:207-220）——**基本正确，但状态词汇有歧义**

- 逻辑：`err != nil` 时追加 `Errors`；若 `updated.Status` 为空或 `"ok"` 则降级为 `"error"`，并写入 `updated.Error`；若 `updated.ID == ""`（采集器返回零值）则回退到 `source` 并标 `"error"`；`Channels` 为空时继承旧栏目（221-223）。测试 `service_test.go:116-155` 覆盖“失败来源保留身份与旧栏目”。
- 问题：crawler 对**非失败**情形也返回 error —— `Status=="empty"`（站点可访问但无文章，crawler.go:318-331）与 `partial`（部分栏目失败）都会 `return ..., errors.New(...)`。于是：
  - 一个健康但“动态加载、无文章”的站点被计入 `sync.Errors`，最终 `phase` 变成 `"partial"`（service.go:144-146），把“信息提示”与“真失败”混为一谈；
  - `updated.Status` 仍是 `empty`/`partial`，但 `updated.Error` 被填上“页面可访问，但未识别出公开文章”，语义上 `Error` 字段既当 warning 又当 error。
  - **D-4（设计取舍）**建议：`CrawlSource` 返回 `(source, articles, warnings []string, err error)` 或引入 `Warning` 字段；`phase` 判定区分 `warnings` 与 `errors`（只有真失败才 partial）。
- 另：`s.state.Sync.Errors` 无上限（每来源 ≥1 条），会随快照一起落盘并按轮次累积到下一次 `Start` 重置（service.go:108）。当前 3 条，风险低，但建议截断（如最多 200 条）并单独存“最后一轮”。

### BUG-6（确认 bug，严重度：中低）`DateInferred` 是死字段，但模型 / UI / CSV / 文案都在承诺“年份推断”

- 事实：
  - `model.Article.DateInferred`（model.go:33）在整个仓库中**没有任何生产端赋值**：`parser.go:659` 构造 `model.Article{...}` 时没有该字段，`crawler.go` 的合并逻辑（199-223、294-303）也从不设置它。实测全库 16753 条 `dateInferred` 全为 `false`。
  - 消费端却完整存在：`frontend/src/api.ts:11` 的 TS 类型、`App.tsx:389` 的「年份推断」徽标与 tooltip（“年份根据采集时间推断”）、`App.tsx:108-109` CSV 的「年份推断」列、`App.tsx:407` 同步弹窗文案「推断的年份会单独提示」。
  - `service.go:231-235` 在保留旧日期时连 `old.DateInferred` 一起带回——逻辑本身正确，但在当前实现下恒为 `false`，是“为未来预留”的空转。
  - `parser_test.go:77` 只断言 `items[1].DateInferred == false`，等于把死字段的正确表现固化成测试。
- 判定：**不是“忘写”，而是有意不推断（README:73 明确“不按当前年份猜测”）后残留的契约**；但对用户/维护者而言这是**确认 bug**：UI 与导出列在承诺一个永不发生的能力，属误导；`service.go:231-235` 的保留分支目前是死代码（无害，但会掩盖未来改动）。
- 影响：无害于数据正确性（反而符合 README 的保守策略），影响的是可理解性与“UI 说了但没做”的一致性；若未来有人补上推断，UI 徽标会立刻生效，但 README 与测试需要同步改。
- 建议（二选一，倾向方案 A）：
  - **A（推荐，符合 README）**：删除 `DateInferred` 字段、`App.tsx` 徽标/tooltip、CSV 列与弹窗措辞，改为展示原始部分日期（已有 `!date && dateText` 分支）；`service.go:231-235` 中 `a.DateInferred = old.DateInferred` 随之删除。
  - **B（若要保留概念）**：在 parser 中实现“月日 + 详情页无年份 → 用 `FetchedAt` 年份且 `DateInferred=true`”，并同步改 README:73 与 `parser_test.go`；注意跨年边界（1 月抓取 12 月文章）必须显式处理，否则会造出错误年份——这正是当前策略不推断的原因。

### BUG-7（确认 bug，严重度：低）跨同步的栏目/标题/摘要“降级覆盖”

- 位置：`service.go:236-239`（`s.state.Articles[i] = a` 整体替换），对比 `crawler.go:212-217`（同一轮内的择优规则）。
- 触发条件：同一 URL 的文章在两轮同步中被不同栏目/不同页面捕获。典型：上轮在「通知通告」栏目页抓到（`Channel="通知通告"`），本轮该栏目页失败或文章已翻页、只在首页列表出现 ⇒ crawler 给 `Channel="首页动态"`，service 整体替换后已保存的栏目被**降级**为「首页动态」。
- 同理：`crawler.go:212-217` 的“标题被截断则用完整标题”规则只在单轮内生效；跨轮时若新标题更短/带省略号，会覆盖旧的好标题。`Content` 只在“新值为空”时保留旧值（service.go:236-238），若新值非空但更短（首页摘要比详情页短），会用短的覆盖长的。
- 影响：数据质量回退，栏目统计与筛选（前端按 channel 过滤）出现漂移；标题被降级会影响搜索与展示。不会崩溃，但会缓慢劣化。
- 建议：把合并规则统一到 service 层（或抽 `mergeArticle(old, new)` 共用函数），按字段择优：`Channel` 非「首页动态」优先；`Title` 非截断优先；`Content` 取更长；`Date` 全日期优先（现有逻辑已正确）。这样 crawler 内部合并与跨轮合并只维护一套规则。

---

## E. HTTP API（`internal/server/server.go`）

### BUG-8（确认 bug，严重度：中低）业务校验错误一律映射为 409 Conflict，客户端无法区分“参数非法”与“任务已在运行”

- 位置：`server.go:34-37`（`s.Start` 任何错误 → 409）。
- 触发条件：`{"pagesPerChannel":501}`、`{"pagesPerChannel":-1}`、`{"maxChannelsPerSource":201}`、`{"sourceId":"not-exist"}` —— 全部返回 409；而 409 的语义应留给“已有同步任务正在运行”（service.go:91）。
- 影响：前端把 409 body 直接 toast（`api.ts:42-48`），用户看到“每栏目页数需在 1 到 50 之间”却配 HTTP 409；自动化脚本/测试无法按状态码分支；与 README:111 的接口表（“已有任务返回 409”）不一致。
- 建议：`Start` 返回可识别错误（`ErrBusy`/`ErrInvalidOptions`/`ErrSourceNotFound`），handler 映射 409 / 400 / 404；`respond` 统一 `{"error": ...}` 已做得不错，保持。

### 其余 API 判定

- **参数校验**：`MaxBytesReader(4096)`（24）、`null`/数组/类型错/尾随垃圾/多文档都已被 `decoder.Decode(&options)` + 二次 `Decode`（26-33）覆盖，测试 `server_test.go:40-67` 完整。缺 `DisallowUnknownFields`（未知字段被静默忽略），属可接受宽松；若希望“拼错字段名”能被发现可开启。越界值在 `Start` 内校验（service.go:82-87），逻辑正确。
- **method 匹配**：Go 1.22 模式路由，`GET /api/snapshot`、`POST /api/sync`、`POST /api/cancel`、`/api/` 兜底返回 JSON 404（48-50），`GET /api/sync` 会落到兜底 404 而非 405 —— 可接受但建议对已知路径的错误方法返回 405 + `Allow`，更符合 REST 预期。`GET /api/snapshot` 也自动接受 `HEAD`（Go 行为），无副作用。
- **`/api/` 精确路径 `/api`**：`ServeMux` 会 301 到 `/api/`，再得 JSON 404，链路正确。
- **静态资源 handler**（51-59）：`/` 挂 `FileServer`，无 SPA fallback（前端用内部 state 导航、无 URL 路由，故实际无影响）；`/api/` 已被更具体的模式优先匹配，不会误落到静态。设置了 `X-Content-Type-Options`/`Referrer-Policy`（53-54），仅对 `/` 设 `Cache-Control: no-cache`（56-57）——`index.html` 会被缓存但同步轮询走 API，可接受；建议对 `index.html` 及 `assets/*` 分别设 `no-cache` 与长缓存（当前未区分，构建产物更新后可能拿到旧 JS）。
- **`sameOrigin`（63-76）**：`Sec-Fetch-Site == "cross-site"` 优先拒绝；`Origin` 存在时校验 scheme/host（含 `u.User == nil`）；`Origin` 缺失则放行。判定正确且测试充分（`server_test.go:69-96` 覆盖跨站/异端口/异协议/`null`/伪造 Origin+跨站）。注意：浏览器同源 `fetch` POST 默认**不发** `Origin`，所以放行分支是正常路径；这同时意味着任何非浏览器客户端（curl/本地脚本）可直接 POST `/api/sync`——按“不考虑安全加固”的范围属**设计取舍 D-10**，README:33 已提示“若部署为公共网站应在反向代理加访问控制”。若未来要收紧，可要求 `Sec-Fetch-Site: same-origin` 或在启动时生成随机 token 注入页面。
- **缺 panic 恢复中间件**：`net/http` 会 recover 单次请求内的 panic（连接断开、日志有栈），不会拖垮进程；但响应不是统一 JSON，前端会拿到网络错误。低危，建议加一层 `recover` 中间件返回 500 JSON（与 BUG-2 的 goroutine panic 是两件事，后者才是致命的）。

---

## F. `cmd/crawl` 与 `cmd/server` 的退出语义

### BUG-9（确认 bug，严重度：中）`cmd/crawl` 的 Ctrl+C 不立即退出，且成功/失败/取消一律退出码 0

- 位置：`cmd/crawl/main.go:34-46`（select 循环）、`32-33`（5 s ticker）、`26-28`（`log.Fatal`）。
- 触发条件：
  1. 运行中按 Ctrl+C：只执行 `svc.Cancel()`（37），随即回到 select **等待下一个 ticker（最长 5 s）**，才打印并 `return`。进程在取消后仍存活 5 s；期间再按 Ctrl+C 只会被 `signal.Notify` 吞掉（buffered chan 容量 1，`defer signal.Stop`），无“二次强杀”。
  2. 同步完成但有 3 条来源错误（如当前数据：70 success + 1 partial + 2 unavailable）：打印“Saved ...; N source warnings”后 `return` ⇒ **退出码 0**；被取消同样是 0；`phase=error`（保存失败）也是 0。CI/脚本无法判定失败。
  3. `-source <无效ID>`：`Start` 返回「未找到指定来源」→ `log.Fatal` ⇒ exit 1，但错误信息不含传给它的 ID，也不提示如何查询可用 ID（README:66 说“来源 ID 可通过 `GET /api/snapshot` 查询”）。
- 影响：批处理脚本/Cron 误判成功；Ctrl+C 后“卡 5 秒”体感差；排障信息不足。
- 建议：
  - 信号处理改为“第一次 Ctrl+C → `Cancel()` 并 `select` 等待 `done`（或把 ticker 改为 200 ms）→ 退出”；第二次信号 `os.Exit(130)`。
  - 退出码约定并写进 README：0 全部成功；2 部分/取消（`phase ∈ {partial, cancelled, error}`）；1 启动失败。
  - `-source` 校验失败时打印该 ID + 前若干个可用 ID，并提示 `GET /api/snapshot`。
  - 顺带：完成时输出 `sync.Errors` 的前若干条，而不只是条数。

### 判定：`cmd/server` 的 `-interval` 定时打印与重叠

- 定时器（`main.go:42-57`）在 ctx 结束时 return，无 goroutine 泄漏；`Start` 失败仅 `log.Print`（51-53），若 crawl 长于 interval，每个 tick 打一条“已有同步任务正在运行”——量很小，可接受，但建议降为 debug 或只在首次出现时打印。
- `-sync` 启动失败（`main.go:37-41`）只 `log.Print` 而**不退出**，服务继续以旧缓存提供页面——对 Web 服务是合理设计（可用性优先），建议在日志中明确“已用本地缓存启动”。

### BUG-10（确认 bug，严重度：低，仓库卫生）README 的检查命令在当前工作区失败

- 证据：`go build ./...` / `go vet ./...` / `go test ./...` 均报
  `research/tmpdiag/{find1571,lib,jxgc*}.go: fetch redeclared / main redeclared`、`research/tmpreview-crawler/**` 多个 `package main`；而 `go test ./internal/...` 通过。
- 影响：README:96-99 列出的 `go test ./...` / `go vet ./...` 会直接失败，新贡献者第一眼即红；临时诊断程序与正式代码混在同一 module（`internal/crawler/zz_probe_test.go` 也会随 `go test ./internal/crawler` 执行）。
- 建议：把 `research/tmp*`、`research/tmpreview-*`、`internal/crawler/zz_probe_test.go` 一类诊断文件移出 module（或整体放进 `research/` 下的独立 module），或给单文件程序加 `//go:build ignore`；CI 用 `go test ./internal/... ./cmd/... .` 覆盖真实代码。

---

## G. 测试缺口（`go test -race` 全绿，但不能说明这些问题不存在）

| 编号 | 缺口 | 证据 / 建议 |
| --- | --- | --- |
| GAP-1 | 没有任何**大数据量**或**锁持有时间**测试/基准：所有 service 测试用 fake collector + 极小状态（`service_test.go` 全文），16.7k 条 / 9.67 MB 下的锁内耗时完全未覆盖 ⇒ BUG-1 无法被 CI 发现 | 增加 `BenchmarkSyncWithLargeState`（构造 ~17k 文章，`t.TempDir()`）+ 断言单次 `Snapshot()` 在同步中的 P99 延迟 |
| GAP-2 | 无 **Close while running** 测试（BUG-3 的挂起路径未覆盖）；无 Close 后 Start 的行为断言 | `TestCloseDuringSyncReturnsAndPersists`，用不响应 ctx 的 fake 验证超时策略 |
| GAP-3 | 无**保存失败**路径测试：只读目录/`Rename` 失败/磁盘满时 `saveLocked` 的错误只被追加进 `Errors`（service.go:147-150、266-268），无测试断言 `phase=="error"` 与内存/磁盘差异 | 用只读目录或损坏的父路径注入失败 |
| GAP-4 | 无 **panic 注入**测试：collector panic 会杀进程（BUG-2），现有测试不会触发 | `TestRunRecoversFromCollectorPanic` |
| GAP-5 | 无**中断恢复**边界测试：只测“启动时把 crawling 改成 interrupted”（`service_test.go:298-322`），没测 `.tmp` 残留、`WriteFile` 与 `Rename` 之间被杀的两种中间态 | 手工构造 `snapshot.json.tmp` + 半写文件，断言启动行为 |
| GAP-6 | `server_test.go` 缺：未知字段、超大数字、`GET /api/sync`（应 405/404 的契约）、空闲时 `/api/cancel`（当前 200）、`/api`（无斜杠）重定向、panic 恢复、`Cache-Control`/`Content-Type` 的负向断言 | 逐条补表驱动用例 |
| GAP-7 | `cmd/server`、`cmd/crawl`、`main.go`/`app.go` 完全无测试：定时器叠加、Ctrl+C、退出码（BUG-4/9）、Wails 损坏缓存退出（BUG-5）均未覆盖 | 把 `main` 拆出可测的 `run(args, ...) error`，用 `exec.Command` 做退出码级 e2e |
| GAP-8 | `DateInferred` 没有任何“生产端”断言：`parser_test.go:77` 只断言 false，等于固化死字段（BUG-6） | 若选方案 A 则删除该断言；选 B 则加推断与跨年用例 |
| GAP-9 | 无**跨轮合并**测试：`service_test.go:63-114` 覆盖了日期保留与栏目保留，但没覆盖“栏目从具体页降级为首页动态”“摘要变短覆盖”的情形（BUG-7） | 两轮 fake crawl 分别返回不同 Channel/Content 长度 |
| GAP-10 | 无 **goroutine 泄漏**或 `-race` 下的长跑测试（4 worker + Discover 并发 + interval ticker） | 加 `goleak` 或退出前 `runtime.NumGoroutine()` 基线断言 |

---

## H. 建议的修复优先级

按“严重度 × 修复成本”排序：

1. **BUG-1**（锁内全量重算/落盘，高）：增量索引 + 计数器 + 锁外去抖保存 —— 收益最大，且是其他超时问题的前置条件。
2. **BUG-2**（run 无 recover，且 defer 谎报完成，中高）：几行代码，消除“桌面端崩溃 + 磁盘状态错误”这一最坏组合。
3. **BUG-3**（Close 无上限，中）+ **BUG-4**（Ctrl+C 不取消抓取，低）：生命周期状态机（closed/busy）+ 有界关闭 + 信号先 `Cancel()`。
4. **BUG-5**（损坏快照让桌面端静默退出，中）：备份后启动，或启动前弹窗提示。
5. **BUG-9**（CLI 退出码 / Ctrl+C 语义，中）、**BUG-8**（409 滥用，中低）、**BUG-6**（DateInferred 契约，中低）、**BUG-7**（跨轮降级，低）、**BUG-10**（research 临时程序破坏 `go test ./...`，低）。
6. 补齐 GAP-1/2/3/4/6/7/8/9 测试；把 README 的 `go test ./...` 恢复为可用状态（或把诊断脚本移出 module / 加 `//go:build ignore`）。

## I. 设计取舍汇总（不是 bug，但需要产品/维护决策）

| 编号 | 取舍 | 位置 | 说明与建议 |
| --- | --- | --- | --- |
| D-1 | 来源 ID 含 URL，目录换网址即生成新 ID，旧条目永久保留 | parser.go:177、service.go:160-170 | 同一单位可能出现两条；建议 `AliasOf`/地址迁移/UI 折叠 |
| D-2 | `href="#"` 的 unavailable 来源永久保留 | parser.go:178-181 | 目录修复后旧条目仍显示“暂无公开站点”，同 D-1 处理 |
| D-3 | `-interval` 定时器与手动同步仅靠 `state.Sync.Running` 串行；用户取消后下个 tick 照常重启 | cmd/server/main.go:42-57、service.go:90-92 | 防止双跑是充分的；但“取消”没有跨 tick 记忆，建议加 `lastManualCancel` 抑制一次自动轮次 |
| D-4 | crawler 用 `error` 同时表达“真失败”和“提示/空站点”，导致空站点也让 `phase=partial` | crawler.go:318-331、service.go:144-146 | 建议 `warnings` 与 `errors` 分离 |
| D-5 | 快照只增不删：站点下线后文章永久保留 | service.go:228-244 | 符合“不因故障删除”，但需要手动清理入口；建议 UI 提供“按来源清理” |
| D-6 | `WriteFile` 后不 `fsync`（Windows 上目录 fsync 也不可行） | service.go:296-300 | 断电可能丢最后一次保存；建议 `f.Sync()` + 双文件轮换降低风险 |
| D-7 | `0600` 在 Windows 上仅影响只读位，实际 ACL 继承父目录 | service.go:297 | 本地单机可接受；跨平台部署需注意 |
| D-8 | `Sync.Errors` 无上限，随快照落盘 | service.go:108、208、267 | 当前 3 条无风险；建议截断并只保留最后一轮 |
| D-9 | 中断后可能残留 `snapshot.json.tmp`，启动不清理 | service.go:296 | 无损坏风险，占 10 MB；建议启动时删除 |
| D-10 | `sameOrigin` 在无 `Origin` 时放行，本地任意进程/脚本可触发同步 | server.go:63-76 | 按本次范围可接受（README:33 已有部署提示）；如需收紧可校验 `Sec-Fetch-Site` 或注入一次性 token |

### 补充说明：为什么 `go test -race` 暴露不了这些问题

- BUG-1 是**锁粒度/复杂度**问题，所有共享访问都在 `s.mu` 保护下，`-race` 只检测“未同步的并发访问”，故 3.7 s 全绿。
- BUG-2 需要 panic 注入才会触发；BUG-3 需要“不响应 ctx 的采集器”；BUG-4/9 在 `cmd/` 层，`internal/...` 的测试根本不会执行到；BUG-5 需要损坏文件 + 桌面端进程；BUG-7 是跨两轮同步的字段择优问题，现有测试只覆盖了“日期不被降级”，没有覆盖 channel/content。
- 现有并发测试（`service_test.go:157-216`）质量不错：16 个 goroutine 竞争 `Start` 只允许一个成功、双重 `Cancel` 幂等、取消后重启，确实覆盖了 `Running` 标志的竞态面；缺的是“Close 与 run 相互等待”“保存失败”“大数据量”三类。

---

## 附录：关键结论速查

1. **不存在数据竞争**（`-race` 全绿，且锁的读写点完整）；真正的并发问题是**锁粒度**：每个来源完成时在写锁内做全量索引、全量排序、10 MB 序列化，累计持锁 6–12 s/轮，读接口被写优先阻塞 0.3–0.8 s。
2. **`Cancel` 后旧 goroutine 不会污染新 run**：新 run 必须等旧 run 置 `Running=false`，且旧 run 之后不再写 state。串行化充分。
3. **`Close()` 是唯一无上界的等待**（`<-done`），在 Wails `OnShutdown` 中同步执行；采集器尊重 ctx 时通常 <1 s，但缺少兜底超时。
4. **持久化本身可靠**（临时文件 + 原子 Rename、损坏保留、中断标记），缺 fsync 与 `.tmp` 清理；`MkdirAll` 空路径不成立（已核实非 bug）。
5. **合并语义总体正确**：Discover 不丢 Channels/Status/ArticleCount；错误来源保留身份与旧栏目；旧日期保留逻辑正确，但 `DateInferred` 是死字段（与 UI 承诺不一致）。
6. **API 校验充分，但状态码语义有偏差**（校验失败返回 409）；无 panic 中间件（低危）；`sameOrigin` 的实现与测试都到位。
7. **CLI/服务退出语义最弱**：`cmd/crawl` 取消后仍等 5 s、错误也返回 0；`cmd/server` Ctrl+C 不先取消抓取。
8. **仓库卫生**：`research/` 下的临时诊断程序会让 README 里的 `go test ./...` / `go vet ./...` 直接失败（BUG-10），日常验证请用 `go test ./internal/...`。
