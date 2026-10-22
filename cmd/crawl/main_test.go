package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cduhub/internal/model"
	"cduhub/internal/service"
)

// fakeService 让 CLI 的退出码语义可以在无网络环境下被完整测试。
type fakeService struct {
	mu          sync.Mutex
	snapshot    model.Snapshot
	startErr    error
	stayRunning bool
	cancelCalls int
	closeCalls  int
	onCancel    func()
}

func (f *fakeService) Start(model.SyncOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.snapshot.Sync.Running = f.stayRunning
	return nil
}

func (f *fakeService) Cancel() {
	f.mu.Lock()
	hook := f.onCancel
	f.cancelCalls++
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (f *fakeService) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
}

func (f *fakeService) Snapshot() model.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeService) finish(phase string, failures []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot.Sync.Running = false
	f.snapshot.Sync.Phase = phase
	f.snapshot.Sync.Errors = failures
}

func newFakeFactory(fake *fakeService) serviceFactory {
	return func(string) (syncService, error) { return fake, nil }
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		fake     *fakeService
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{
			name:     "success",
			fake:     &fakeService{snapshot: model.Snapshot{Sources: []model.Source{{ID: "office"}}, Sync: model.SyncStatus{Phase: "completed"}}},
			wantCode: exitOK,
			wantOut:  "phase=completed",
		},
		{
			name: "partial failure",
			fake: &fakeService{snapshot: model.Snapshot{
				Sources: []model.Source{{ID: "office"}},
				Sync:    model.SyncStatus{Phase: "partial", Errors: []string{"bad: timeout"}},
			}},
			wantCode: exitFailure,
			wantOut:  "bad: timeout",
		},
		{
			// 官方目录里「只有 # 链接」的单位会写进 Sync.Errors，但那是已知情况，
			// 不该让每次全量同步都以退出码 1 结束。
			name: "only unavailable directory entries",
			fake: &fakeService{snapshot: model.Snapshot{
				Sources: []model.Source{{ID: "office"}, {ID: "venue", Status: "unavailable"}},
				Sync: model.SyncStatus{Phase: "partial", Errors: []string{
					"体育场馆中心: 官网目录未提供可访问的网址",
					"化学化工学院: 官方目录未提供可访问的网址",
				}},
			}},
			wantCode: exitOK,
			wantOut:  "未提供可访问的网址",
		},
		{
			name:     "error phase",
			fake:     &fakeService{snapshot: model.Snapshot{Sync: model.SyncStatus{Phase: "error", Errors: []string{"保存失败: disk full"}}}},
			wantCode: exitFailure,
			wantOut:  "保存失败",
		},
		{
			name:     "invalid options",
			fake:     &fakeService{startErr: fmt.Errorf("%w：每栏目页数需在 1 到 50 之间", service.ErrInvalidOptions)},
			args:     []string{"-pages", "99"},
			wantCode: exitFailure,
			wantErr:  "启动同步失败",
		},
		{
			name: "unknown source id",
			fake: &fakeService{
				startErr: fmt.Errorf("%w：nope", service.ErrSourceNotFound),
				snapshot: model.Snapshot{Sources: []model.Source{{ID: "office"}, {ID: "college"}}},
			},
			args:     []string{"-source", "nope"},
			wantCode: exitFailure,
			wantErr:  "未找到来源",
		},
		{
			// 参数解析失败时不应创建服务，也就没有 Close 可断言，故 fake 留空。
			name:     "bad flag",
			args:     []string{"-pages", "not-a-number"},
			wantCode: exitFailure,
			wantErr:  "invalid value",
		},
		{
			name:     "factory failure",
			fake:     nil,
			wantCode: exitFailure,
			wantErr:  "读取本地数据失败",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			factory := serviceFactory(func(string) (syncService, error) {
				if tc.fake == nil {
					return nil, errors.New("cache corrupted")
				}
				return tc.fake, nil
			})
			code := run(tc.args, &stdout, &stderr, factory, nil)
			if code != tc.wantCode {
				t.Fatalf("exit code=%d want %d; stdout=%q stderr=%q", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if tc.wantOut != "" && !strings.Contains(stdout.String(), tc.wantOut) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantErr)
			}
			if tc.fake != nil {
				tc.fake.mu.Lock()
				defer tc.fake.mu.Unlock()
				if tc.fake.closeCalls != 1 {
					t.Errorf("service was not closed exactly once: %d", tc.fake.closeCalls)
				}
			}
		})
	}
}

func TestRunInterruptCancelsImmediatelyAndReturns130(t *testing.T) {
	interrupt := make(chan os.Signal, 1)
	fake := &fakeService{
		stayRunning: true,
		snapshot:    model.Snapshot{Sync: model.SyncStatus{Phase: "crawling", Total: 73, Completed: 10}},
	}
	fake.onCancel = func() { fake.finish("cancelled", nil) }
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	begin := time.Now()
	go func() { done <- run(nil, &stdout, &stderr, newFakeFactory(fake), interrupt) }()

	// 等 run 进入等待循环后再发信号。
	time.Sleep(100 * time.Millisecond)
	interrupt <- os.Interrupt

	select {
	case code := <-done:
		if code != exitCancelled {
			t.Fatalf("cancelled run should exit 130, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not exit promptly after Ctrl+C (still waiting for a 5s tick?)")
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %v, expected an immediate exit", elapsed)
	}
	if fake.cancelCalls != 1 {
		t.Fatalf("service cancel called %d times, want 1", fake.cancelCalls)
	}
	if !strings.Contains(stdout.String(), "同步已取消") {
		t.Fatalf("cancellation result was not reported: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "正在取消同步") {
		t.Fatalf("cancellation progress was not reported: %q", stderr.String())
	}
}
