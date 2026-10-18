// Command crawl performs an explicit batch synchronization without opening a UI.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"cduhub/internal/model"
	"cduhub/internal/service"
)

func main() {
	data := flag.String("data", "data/snapshot.json", "Local JSON database")
	pages := flag.Int("pages", 2, "Pages per channel (1-50)")
	channels := flag.Int("channels", 40, "Channels per source (1-100)")
	source := flag.String("source", "", "Optional source ID; empty discovers all official sources")
	flag.Parse()
	svc, err := service.New(*data)
	if err != nil {
		log.Fatal(err)
	}
	defer svc.Close()
	if err = svc.Start(model.SyncOptions{SourceID: *source, PagesPerChannel: *pages, MaxChannelsPerSource: *channels}); err != nil {
		log.Fatal(err)
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-interrupt:
			svc.Cancel()
		case <-ticker.C:
			s := svc.Snapshot()
			log.Printf("%s %d/%d · %s · %d sources · %d articles", s.Sync.Phase, s.Sync.Completed, s.Sync.Total, s.Sync.Current, len(s.Sources), len(s.Articles))
			if !s.Sync.Running {
				log.Printf("Saved to %s; %d source warnings", *data, len(s.Sync.Errors))
				return
			}
		}
	}
}
