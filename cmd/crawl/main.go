// Command crawl performs an explicit batch synchronization without opening a UI.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"cduhub/internal/model"
	"cduhub/internal/service"
)

// 退出码约定：0 全部成功，1 失败（启动失败或本轮有错误来源），130 用户取消。
const (
	exitOK        = 0
	exitFailure   = 1
	exitCancelled = 130
)

const (
	// progressInterval 是正常抓取时的进度上报间隔。
	progressInterval = time.Second
	// shutdownPollInterval 是取消后的收尾轮询间隔：远小于进度周期，
	// 保证 Ctrl+C 后立即退出，不用像旧实现那样等下一个 5s tick。
	shutdownPollInterval = 200 * time.Millisecond
	// maxReportedErrors 限制退出时打印的错误条数，避免刷屏。
	maxReportedErrors = 10
)

// syncService 抽象出 CLI 需要的服务能力，便于在没有网络的情况下测试退出码语义。
type syncService interface {
	Start(model.SyncOptions) error
	Cancel()
	Close()
	Snapshot() model.Snapshot
}

type serviceFactory func(path string) (syncService, error)

func main() {
	interrupt := make(chan os.Signal, 2)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, newSyncService, interrupt))
}

func newSyncService(path string) (syncService, error) { return service.New(path) }

// run 承载全部逻辑并返回进程退出码，便于用 fake 服务做退出码级测试。
func run(args []string, stdout, stderr io.Writer, factory serviceFactory, interrupt <-chan os.Signal) int {
	flags := flag.NewFlagSet("crawl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	data := flags.String("data", "data/snapshot.json", "Local JSON database")
	pages := flags.Int("pages", 2, "Pages per channel (1-50)")
	channels := flags.Int("channels", 40, "Channels per source (1-100)")
	source := flags.String("source", "", "Optional source ID; empty discovers all official sources")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFailure
	}

	svc, err := factory(*data)
	if err != nil {
		fmt.Fprintf(stderr, "读取本地数据失败: %v\n", err)
		return exitFailure
	}
	defer svc.Close()

	options := model.SyncOptions{SourceID: *source, PagesPerChannel: *pages, MaxChannelsPerSource: *channels}
	if err := svc.Start(options); err != nil {
		fmt.Fprintf(stderr, "启动同步失败: %v\n", err)
		if errors.Is(err, service.ErrSourceNotFound) {
			printKnownSources(stderr, svc.Snapshot(), *source)
		}
		return exitFailure
	}
	return waitSync(stdout, stderr, svc, interrupt, *data)
}

// waitSync 等待本轮结束，同时上报进度并处理 Ctrl+C：
// 第一次中断立即取消并继续等待收尾（打印结果后以 130 退出），第二次中断直接立即退出。
func waitSync(stdout, stderr io.Writer, svc syncService, interrupt <-chan os.Signal, dataPath string) int {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	lastProgress := ""
	cancelling := false
	for {
		snapshot := svc.Snapshot()
		if !snapshot.Sync.Running {
			printSummary(stdout, dataPath, snapshot, cancelling)
			if cancelling || snapshot.Sync.Phase == "cancelled" {
				return exitCancelled
			}
			if snapshot.Sync.Phase == "completed" && len(snapshot.Sync.Errors) == 0 {
				return exitOK
			}
			// 官方目录里本来就有「只有 # 链接、没有站点」的单位（README 已说明），
			// 它们会作为提示写进 Sync.Errors。这类已知情况不算本轮失败，否则每次
			// 全量同步都会以退出码 1 结束。
			if onlyUnavailableSourcesFailed(snapshot) {
				return exitOK
			}
			return exitFailure
		}
		if cancelling {
			select {
			case <-interrupt:
				fmt.Fprintln(stderr, "再次收到中断信号，跳过收尾直接退出（已完成的来源此前已保存）")
				return exitCancelled
			case <-time.After(shutdownPollInterval):
			}
			continue
		}
		select {
		case <-interrupt:
			cancelling = true
			fmt.Fprintln(stderr, "正在取消同步，等待已经完成的来源收尾…")
			svc.Cancel()
		case <-ticker.C:
			if progress := progressLine(snapshot); progress != lastProgress {
				fmt.Fprintln(stdout, progress)
				lastProgress = progress
			}
		}
	}
}

func progressLine(snapshot model.Snapshot) string {
	return fmt.Sprintf("%s %d/%d · %s · %d 来源 · %d 文章", snapshot.Sync.Phase, snapshot.Sync.Completed, snapshot.Sync.Total, snapshot.Sync.Current, len(snapshot.Sources), len(snapshot.Articles))
}

// onlyUnavailableSourcesFailed 报告本轮是否只出现「官方目录没有提供站点」这类
// 已知提示。种子快照里带 2 个 href="#" 的单位，全量同步必然产生这两条提示；
// 把它们当成失败会让退出码失去区分度（0 与 1 都变成 1）。
func onlyUnavailableSourcesFailed(snapshot model.Snapshot) bool {
	if len(snapshot.Sync.Errors) == 0 {
		return false
	}
	for _, message := range snapshot.Sync.Errors {
		if !strings.Contains(message, "未提供可访问的网址") {
			return false
		}
	}
	return true
}

func printSummary(stdout io.Writer, dataPath string, snapshot model.Snapshot, cancelled bool) {
	if cancelled || snapshot.Sync.Phase == "cancelled" {
		fmt.Fprintln(stdout, "同步已取消；已经完成的来源数据已保存。")
	}
	fmt.Fprintf(stdout, "Saved to %s; phase=%s · %d/%d sources · %d articles · %d warnings\n",
		dataPath, snapshot.Sync.Phase, snapshot.Sync.Completed, snapshot.Sync.Total, len(snapshot.Sources), len(snapshot.Sync.Errors))
	fmt.Fprintf(stdout, "文章总数 %d，来源总数 %d。\n", len(snapshot.Articles), len(snapshot.Sources))
	if len(snapshot.Sync.Errors) == 0 {
		return
	}
	fmt.Fprintf(stdout, "本轮共 %d 条错误/提示：\n", len(snapshot.Sync.Errors))
	for i, message := range snapshot.Sync.Errors {
		if i == maxReportedErrors {
			fmt.Fprintf(stdout, "  … 其余 %d 条省略\n", len(snapshot.Sync.Errors)-maxReportedErrors)
			break
		}
		fmt.Fprintf(stdout, "  - %s\n", message)
	}
}

func printKnownSources(stderr io.Writer, snapshot model.Snapshot, requested string) {
	if len(snapshot.Sources) == 0 {
		fmt.Fprintln(stderr, "本地还没有已知来源，请先不带 -source 执行一次全量同步（或先运行网站版点一次「立即同步」）。")
		return
	}
	const limit = 10
	ids := make([]string, 0, limit)
	for _, source := range snapshot.Sources {
		if len(ids) == limit {
			break
		}
		ids = append(ids, source.ID)
	}
	fmt.Fprintf(stderr, "未找到来源 %q。可用来源 ID（前 %d 个）: %s\n", requested, len(ids), strings.Join(ids, ", "))
	fmt.Fprintln(stderr, "完整列表可通过 GET /api/snapshot 查询。")
}
