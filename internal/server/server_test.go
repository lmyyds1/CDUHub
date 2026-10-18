package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"cduhub/internal/model"
	"cduhub/internal/service"
)

type countingCollector struct{ calls atomic.Int32 }

func (c *countingCollector) Discover(ctx context.Context) ([]model.Source, []string) {
	c.calls.Add(1)
	<-ctx.Done()
	return nil, nil
}

func (*countingCollector) CrawlSource(context.Context, model.Source, model.SyncOptions) (model.Source, []model.Article, error) {
	return model.Source{}, nil, nil
}

func testHandler(t *testing.T) (http.Handler, *service.Service, *countingCollector) {
	t.Helper()
	collector := &countingCollector{}
	s, err := service.NewWithCollector("", collector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return Handler(s, fstest.MapFS{"index.html": {Data: []byte("<title>CDU 信息汇总</title>")}}), s, collector
}

func TestSyncRejectsMalformedRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"invalid JSON", "{"},
		{"wrong value type", `{"pagesPerChannel":"two"}`},
		{"array", `[]`},
		{"oversized", `{"sourceId":"` + strings.Repeat("a", 5000) + `"}`},
		{"trailing garbage", `{} trailing`},
		{"multiple JSON documents", `{} {}`},
		{"null options", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, s, _ := testHandler(t)
			request := httptest.NewRequest(http.MethodPost, "http://localhost:34115/api/sync", strings.NewReader(tc.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Errorf("malformed request status=%d, want 400; body=%s", response.Code, response.Body.String())
			}
			if s.Snapshot().Sync.Running {
				t.Error("malformed request launched a crawl")
			}
		})
	}
}

func TestMutationRejectsCrossSiteRequests(t *testing.T) {
	for _, endpoint := range []string{"/api/sync", "/api/cancel"} {
		for _, tc := range []struct {
			name   string
			origin string
			site   string
		}{
			{"other host", "https://example.org", ""},
			{"other port", "http://localhost:9999", ""},
			{"other scheme", "https://localhost:34115", ""},
			{"opaque origin", "null", ""},
			{"cross-site fetch", "", "cross-site"},
			{"forged matching origin with cross-site fetch", "http://localhost:34115", "cross-site"},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				handler, s, _ := testHandler(t)
				request := httptest.NewRequest(http.MethodPost, "http://localhost:34115"+endpoint, strings.NewReader(`{}`))
				request.Header.Set("Origin", tc.origin)
				request.Header.Set("Sec-Fetch-Site", tc.site)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden || s.Snapshot().Sync.Running {
					t.Fatalf("cross-site mutation was accepted: status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestSameOriginSyncAndSnapshotContract(t *testing.T) {
	handler, s, _ := testHandler(t)
	request := httptest.NewRequest(http.MethodPost, "http://localhost:34115/api/sync", strings.NewReader(`{"pagesPerChannel":1,"maxChannelsPerSource":3}`))
	request.Header.Set("Origin", "http://localhost:34115")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !s.Snapshot().Sync.Running {
		t.Fatalf("same-origin sync rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost:34115/api/snapshot", nil))
	var snapshot model.Snapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || !snapshot.Sync.Running {
		t.Fatalf("snapshot does not expose the accepted crawl: status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("incorrect snapshot headers: %v", response.Header())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://localhost:34115/api/sync", strings.NewReader(`{}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("overlapping sync should return 409, got %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://localhost:34115/api/cancel", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("same-origin cancellation returned %d", response.Code)
	}
}

func TestAPIUnknownRouteAndFrontendAsset(t *testing.T) {
	handler, _, _ := testHandler(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost:34115/api/missing", nil))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unknown API fell through to frontend: status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost:34115/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "CDU 信息汇总") || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("frontend not served correctly: status=%d body=%s", response.Code, response.Body.String())
	}
}
