# 团队修复记录（第二轮：非日期问题）

第一轮的日期识别修复见 [SUMMARY.md](SUMMARY.md)。本轮按审查报告修复其余已确认问题，四个任务并行执行，全部改动已落盘并通过完整验证。

## 任务与分工

| 任务 | 负责人 | 范围 | 状态 |
| --- | --- | --- | --- |
| task-5 | backend-fixer | `internal/service/`、`internal/server/`、`cmd/` | 完成 |
| task-6 | frontend-fixer | `frontend/src/` | 完成 |
| task-7 | crawler-fixer | `internal/crawler/` | 完成 |
| task-8 | deadfield-fixer | `internal/model/`、service/parser 测试、README | 完成 |

## 一、后端（task-5）

| 项 | 问题 | 改法 | 实测 |
| --- | --- | --- | --- |
| 写锁内开销 | 每来源完成都在写锁内重建全量索引 map、全量扫 ArticleCount、`SliceStable` 全量排序、10 MB `MarshalIndent` | 常驻 `articleIndex`/`sourceCounts` 增量维护；排序移出写锁；`saveLoop` 500 ms 去抖 + `saveMu` 串行 + `stateSeq` 版本号 | 锁内成本 2.35 ms → **8.8 µs**（≈250×）；写盘 73 次/轮 → ≈14 次/轮；累计持锁 6–12 s → 可忽略 |
| panic 恢复 | collector panic 杀进程，且旧 defer 先把 phase 写成 `completed` 并落盘 | `finishRun` 统一 `recover`；collector 调用点 + `processSource` 双层 recover；panic → phase=`error` | 新增 2 个 panic 注入测试，重启后磁盘 phase 仍为 error |
| Close 无上限 | Wails 退出可能挂起 | `closeWithTimeout(5s)` + `closed` 标志；Cancel/Close 幂等 | 故意忽略 ctx 的 collector 下，200 ms 上限即返回 |
| 状态码 | 所有业务错误一律 409 | 4 个哨兵错误 + `statusForError` 映射 400/404/409/500；`withRecovery` 中间件 | 真机 curl：`pagesPerChannel=501`→**400**，`sourceId=nope`→**404** |
| CLI 退出码 | 成功/失败/取消一律 0；Ctrl+C 要等 5 s tick | `run()` 返回 0/1/130；Ctrl+C 立即 Cancel + 200 ms 轮询收尾；无效 source 打印可用 ID | 真机：无效 source→1，正常→0，Ctrl+C→130（<3 s） |
| SIGINT 顺序 | 先 Shutdown 后 Cancel，抓取拖住退出 | 先 `svc.Cancel()` 再 `Shutdown`，错误改为 `log.Printf` | 无 e2e（Windows 无法对子进程发 os.Interrupt），已手工验证 |

额外：`Sync.Errors` 加 200 条上限；启动时清理残留 `.tmp`；`persist` 补 `f.Sync()`。

## 二、前端（task-6）

| 项 | 改法 | 实测 |
| --- | --- | --- |
| 排序热路径 | 模块级 `zhCollator`，日期先比字符串，同日期才用 collator 比标题 | **525 ms → 11.4 ms（≈46×）**；18342 条输出顺序与旧实现逐条一致 |
| filter 内重复计算 | `todayDay` 提到组件体；`todayCount` 改 `useMemo`；`Map<id, normalizedText>` 预索引（仅集合变化时构建 27 ms） | contains「通知」26.6 → **8.5 ms/键**；fuzzy 长查询 178.8 → **120.6 ms/键** |
| 输入卡顿 | `useDeferredValue(query)` 进过滤依赖，输入框即时回显 | 命中数与旧实现逐 id 完全一致 |
| api 超时 | `AbortSignal.timeout(8000)` 优先 + `AbortController` 回退；bridge 分支加 Promise 竞速 | 6 类异常体（对象型 error / 纯文本 / HTML / 空体 / 非法 JSON）均产出合理中文，无 `[object Object]` |
| 未来日期 | 排序 tier：正常 → 未知 → 未来（数据保留）；`<time title>` 提示 | 注入 2 条 2026-12-31 验证落到末尾 |
| 移动端 a11y | `sidebarRef` + `inert` + `aria-hidden`，媒体查询联动 | ≤480 px 不再隐藏日期提示 |
| 健壮性 | `main.tsx` 加 ErrorBoundary | 白屏改为「界面出错，请刷新」 |

## 三、爬虫（task-7）

| 项 | 问题 | 改法 |
| --- | --- | --- |
| 详情预算 | `detailBudget=12`，且只挑 `Date==""`，列表页已写错的日期永不纠正 | 12 → 40（按文章数收口）；新增 `detailCandidates()` 按「标题截断 > 无日期 > 有日期」排序；`detailMetadataInfo()` 返回 `DateExplicit` 信号，**只有显式发布标记才允许覆盖列表日期** |
| 栏目误判 | 同行带日期的栏目链接被当文章丢弃 | 删除该同行日期否决（`articleURL` 更早分支已排除真文章） |
| 导航页误收录 | `index.htm`、`xzzq1.htm`、`bszn.htm`、面包屑「首页」被收录 | `navigationTitle` / `navigationPageURL` / `insideNavigationChrome` 三重收紧，仅在 `!articleURL` 时生效 |
| 详情标题 | 站点名 `<h1>` 盖过 `.art-tit` 内真标题 | `.art-tit/.cont-tit/.news-title` 内 h2-h5 优先，h1 仅兜底且等于站点名时丢弃 |
| 排除区 | 只排除 footer/friendlink | 扩到 position/breadcrumb/crumb/copyright/nexthtml；详情标题再加 nav/menu/sidebar/foot |

新增 8 个测试（`internal/crawler/parser_hardening_test.go`），含真实 `research/journal-detail.html` 断言。

## 四、死字段清理（task-8）

`model.Article.DateInferred` 及其在 `service.go`、两处测试断言中的引用全部删除；`internal/ cmd/ frontend/src/ README.md` 下 `DateInferred|dateInferred|年份推断` **0 命中**（`research/review/` 为历史审查记录，`frontend/wailsjs/go/models.ts` 为 Wails 生成产物，均按要求保留）。

## 五、验证结果

```
go build ./...            ✅
go vet ./...              ✅
go test -count=1 ./...    ✅  (crawler / server / service / cmd/crawl 全绿)
go test -race ./internal/... ✅
gofmt -l internal cmd     ✅ 空
npx tsc -b --force        ✅ exit 0
```

真实站点端到端：

- `GET /api/snapshot` → 200，73 来源 / 18342 文章
- `POST /api/sync {"pagesPerChannel":501}` → **400**
- `POST /api/sync {"sourceId":"nope"}` → **404**
- `crawl -source nope` → 退出码 1 + 可用来源 ID 列表
- `crawl -source <有效>` → 退出码 0
- 全量重抓 18342 篇：**丢失日期 0 条**，新增 271 条，修正 58 条（逐条核对页面原文，如 news 44 条由列表页近似日改为详情页 `发布时间`）；no-date 从 21.1% 降到 **8.7%**；未来日期 **0** 条；标题夹带日期 490 → 67（剩余均来自未重抓的旧页面，本次新抓 0 条）

## 六、明确未做 / 已知取舍

1. **BUG-7 跨轮合并择优**未做：改 service 合并规则会动到「刷新更新标题」的既有测试语义，建议单独立项。
2. **cmd/server 的 SIGINT 顺序**无自动化测试：Windows 下无法对子进程发 `os.Interrupt`，只做了构建 + 手工验证。
3. **`saveDebounce=500 ms`**：崩溃最多丢失最近 500 ms 完成来源；正常退出/取消/Close 强制同步落盘，不丢数据。若改成 0 会退回「每站写一次 ≈700 MB/轮」。
4. **`.tmp` 启动清理**是新行为：会删除 `path+".tmp"` 残留（我们自己的临时文件），语义安全但属新增。
5. **`npm run build`** 在本机沙箱因 esbuild `spawn EPERM` 失败，属环境限制，未改构建配置；`tsc -b` 通过，`dist/` 未被写入。
6. **`frontend/wailsjs/go/models.ts`** 仍含 `dateInferred`，是 Wails 自动生成的绑定产物，下次 `wails build` 会重新生成。
