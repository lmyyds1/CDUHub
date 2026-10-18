package main

import (
	"context"
	"errors"
	"net/url"

	"cduhub/internal/model"
	"cduhub/internal/service"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx     context.Context
	service *service.Service
}

func (a *App) startup(ctx context.Context)               { a.ctx = ctx }
func (a *App) shutdown(ctx context.Context)              { a.service.Close() }
func (a *App) GetSnapshot() model.Snapshot               { return a.service.Snapshot() }
func (a *App) StartSync(options model.SyncOptions) error { return a.service.Start(options) }
func (a *App) CancelSync()                               { a.service.Cancel() }
func (a *App) OpenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("无效网页地址")
	}
	runtime.BrowserOpenURL(a.ctx, raw)
	return nil
}
