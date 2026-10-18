package main

import (
	"log"
	"os"
	"path/filepath"

	"cduhub/frontend"
	"cduhub/internal/service"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

func main() {
	dataPath := os.Getenv("CDU_DATA_PATH")
	if dataPath == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			log.Fatal(err)
		}
		dataPath = filepath.Join(dir, "CDUHub", "snapshot.json")
	}
	svc, err := service.New(dataPath)
	if err != nil {
		log.Fatal(err)
	}
	app := &App{service: svc}
	err = wails.Run(&options.App{Title: "成大信息汇 · 成都大学校园信息聚合", Width: 1440, Height: 960, MinWidth: 1024, MinHeight: 720, BackgroundColour: &options.RGBA{R: 247, G: 249, B: 248, A: 255}, AssetServer: &assetserver.Options{Assets: frontend.Assets}, OnStartup: app.startup, OnShutdown: app.shutdown, Bind: []interface{}{app}})
	if err != nil {
		log.Fatal(err)
	}
}
