package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"cduhub/internal/model"
	"cduhub/internal/service"
)

func Handler(s *service.Service, assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) { respond(w, http.StatusOK, s.Snapshot()) })
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			respond(w, http.StatusForbidden, map[string]string{"error": "不允许跨站操作"})
			return
		}
		var options *model.SyncOptions
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&options); err != nil || options == nil {
			respond(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			respond(w, http.StatusBadRequest, map[string]string{"error": "请求必须是单个 JSON 对象"})
			return
		}
		if err := s.Start(*options); err != nil {
			respond(w, statusForError(err), map[string]string{"error": err.Error()})
			return
		}
		respond(w, http.StatusAccepted, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			respond(w, http.StatusForbidden, map[string]string{"error": "不允许跨站操作"})
			return
		}
		s.Cancel()
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
	})
	files := http.FileServer(http.FS(assets))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
	return withRecovery(mux)
}

// withRecovery 兜住 handler 内的 panic，返回统一的 JSON 500，
// 而不是让连接被直接掐断、前端只看到网络错误。
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				respond(w, http.StatusInternalServerError, map[string]string{"error": "服务器内部错误"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusForError 把 service 的哨兵错误映射为 HTTP 状态码：
// 409 任务已在运行/服务已关闭，400 参数非法，404 来源不存在，其余 500。
func statusForError(err error) int {
	switch {
	case errors.Is(err, service.ErrSyncRunning), errors.Is(err, service.ErrClosed):
		return http.StatusConflict
	case errors.Is(err, service.ErrInvalidOptions):
		return http.StatusBadRequest
	case errors.Is(err, service.ErrSourceNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return err == nil && u.Scheme == scheme && u.User == nil && strings.EqualFold(u.Host, r.Host)
	}
	return true
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
