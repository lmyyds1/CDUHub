# 成大信息汇

基于 **Go + Wails v2 + React / TypeScript** 的成都大学公开信息聚合应用。同一套界面可作为本地网站运行，也可编译为 Windows 桌面程序。

从以下官方目录自动发现来源，不依赖手工维护的部门、学院清单：

- [组织机构](https://www.cdu.edu.cn/zzjg.htm)
- [院系设置](https://www.cdu.edu.cn/yxsz.htm)

逐站发现通知公告、新闻动态、领导动态、业务流程、党务工作、校务工作、教学科研、招生就业等栏目，提取**标题、发布日期、原文网址、来源、栏目和受限长度的公开摘要**。支持包含匹配、模糊匹配和中文同义词智能搜索（同时检索摘要），可组合“只看来源”和“不看来源”筛选；另有部门/学院/具体来源/栏目/日期筛选、时间排序、收藏、CSV 导出、单站重试、取消同步和增量持久化。

## 启动网站

要求：Go 1.23+、Node.js 20.19+（或更新的受支持版本）。首次安装依赖需要网络。

```powershell
cd frontend
npm install
npm run build
cd ..
go mod download
go run ./cmd/server
```

打开 <http://127.0.0.1:8080>。点击「立即同步」从官方目录开始采集；已有 `data/snapshot.json` 时立即显示本地缓存。页面不会将演示数据混入实际结果。

后台启动时自动同步、每 6 小时刷新：

```powershell
go run ./cmd/server -sync -pages 2 -interval 6h
```

这是运行中的服务定时器，关闭服务后停止。可用 `-addr 127.0.0.1:8081` 改端口，用 `-data <路径>` 指定数据库。默认仅监听本机；若部署为公共网站，应在反向代理中增加同步接口的访问控制，避免任何访问者启动采集任务。

## Wails 桌面端

要求 Wails v2 CLI；Windows 需要 WebView2 Runtime。

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
wails doctor
wails dev
```

生成可执行文件：

```powershell
wails build
```

产物位于 `build/bin/CDUHub.exe`。桌面端默认将数据保存到 `%APPDATA%\CDUHub\snapshot.json`。通过 `CDU_DATA_PATH` 可以指定与网站相同的数据文件：

```powershell
$env:CDU_DATA_PATH = Join-Path (Get-Location) 'data/snapshot.json'
.\build\bin\CDUHub.exe
```

**不要同时让多个进程写入同一数据文件**。桌面端和网站默认使用各自的数据目录；收藏保存在各自 WebView/浏览器的 localStorage 中。

## 无界面批量采集

```powershell
go run ./cmd/crawl -pages 2 -channels 40
```

可指定 `-source <来源ID>` 仅更新一个已发现的来源，或 `-data <路径>` 保存到其他数据库。来源 ID 可通过 `GET /api/snapshot` 查询。每站完成后保存；Ctrl+C 取消时保留已经完成的来源数据。

## 覆盖范围与日期

- 2026-09-14 的官方目录包含 43 个部门条目和 30 个院系条目，包括下属学院、共用网站的不同单位。它们是 **73 个目录归属条目**，不是 73 个独立网站。
- 「体育场馆中心」「化学化工学院」当时均只有 `#` 链接，界面保留来源并标注不可用。不会猜测网址。
- 默认每栏目读取 2 页、每站最多发现 40 个栏目；界面可设为 1–50 页、1–100 个栏目。会跟随下一页并增量合并。**默认同步不代表所有历史文章已全部抓取**。
- 日期仅取页面实际显示的发布日期；支持 `2026-09-14`、`2026年9月14日`、分块日期等。只有 `09-14` 时尝试详情页补全；仍无年份则保留原始月日，不按当前年份猜测。缺少完整日期的记录会显示说明，且不计入限定日期范围的结果。
- 自动发现依赖公开 HTML 中的链接。动态脚本加载、验证码、登录页、TLS/网络错误或特殊页面结构可能无法采集，来源和同步记录会显示说明。程序不绕过认证或验证码。
- 请求限制在成都大学官方域名，保留 TLS 校验，限制并发、单站请求频率、响应大小和请求超时。只保存元数据，不保存文章全文和附件。
- 同站按原文网址去重，不同目录归属保留各自来源标记；统计条数可能包含共用网站在不同单位下的归属记录。既有来源和历史记录不会因单次目录/网站故障被删除。

## 项目结构

```text
main.go / app.go         Wails 入口与 Go 方法绑定
cmd/server/             浏览器网站入口
cmd/crawl/              命令行批量采集
internal/crawler/       官方目录、栏目、文章和日期解析
internal/model/         共享 JSON 数据模型
internal/service/       并发任务、取消、去重和原子文件保存
internal/server/        HTTP API 与内嵌静态资源
frontend/               React 界面、Wails/HTTP 双适配
research/               官方页面快照与覆盖核对说明
```

前端开发：先运行 `go run ./cmd/server`，再到 `frontend` 中运行 `npm run dev`。Vite 会将 `/api` 请求代理到 `127.0.0.1:8080`。Wails 开发则使用 `wails dev`，自动连接 Go 绑定。

## 检查

```powershell
go test ./...
go test -race ./internal/...
go vet ./...
cd frontend
npm run build
```

测试覆盖真实目录结构、嵌套/无效/共用网址、栏目与翻页、日期与详情补全、失败隔离、并发启动、取消重启、缓存恢复、跨站请求与无效 JSON。

## HTTP 接口

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/snapshot` | 来源、文章、同步状态 |
| POST | `/api/sync` | 启动同步，返回 202；已有任务返回 409 |
| POST | `/api/cancel` | 请求取消同步 |

同步参数示例：

```json
{"sourceId":"","pagesPerChannel":2,"maxChannelsPerSource":40}
```

空 `sourceId` 表示重新发现全部目录并同步；缺省/0 页数和栏目数使用默认值。持久化文件损坏时会报错并保留原文件，不静默覆盖。
