package main

import (
	"context"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"cduhub/frontend"
	"cduhub/internal/model"
	"cduhub/internal/server"
	"cduhub/internal/service"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	data := flag.String("data", "data/snapshot.json", "Local JSON database path")
	syncOnStart := flag.Bool("sync", false, "Start a crawl on startup")
	pages := flag.Int("pages", 2, "Maximum pages per channel")
	interval := flag.Duration("interval", 0, "Optional automatic refresh interval, e.g. 6h")
	flag.Parse()
	svc, err := service.New(*data)
	if err != nil {
		log.Fatal(err)
	}
	defer svc.Close()
	assets, err := fs.Sub(frontend.Assets, "dist")
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *syncOnStart {
		if err := svc.Start(model.SyncOptions{PagesPerChannel: *pages}); err != nil {
			log.Print(err)
		}
	}
	if *interval > 0 {
		go func() {
			ticker := time.NewTicker(*interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := svc.Start(model.SyncOptions{PagesPerChannel: *pages}); err != nil {
						log.Print(err)
					}
				}
			}
		}()
	}
	srv := &http.Server{Addr: *addr, Handler: server.Handler(svc, assets), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		// 先取消正在进行的抓取，再关 HTTP：否则长抓取会把退出拖到 Shutdown 超时之后。
		svc.Cancel()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Printf("HTTP 服务关闭未在超时内完成: %v", err)
		}
	}()
	log.Printf("成大信息汇已启动: http://%s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Print(err)
	}
}
