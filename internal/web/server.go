// Package web serves the JSON API and the HTML interface.
package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"paperfull/internal/classify"
	"paperfull/internal/ingest"
	"paperfull/internal/store"
)

//go:embed assets
var assets embed.FS

type Server struct {
	Store    *store.Store
	Ingester *ingest.Ingester
	Cls      *classify.Classifier
	Token    string // empty = no authentication
	// ConsumeDir is shown in the UI so people know where to drop scans.
	ConsumeDir string
	Log        *slog.Logger

	tmpl   *template.Template
	events hub
}

const cookieName = "paperfull_session"

func (s *Server) Handler() http.Handler {
	s.tmpl = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "assets/*.html"))
	mux := http.NewServeMux()
	if s.Ingester != nil {
		s.Ingester.OnTask = s.taskEvent
	}
	mux.HandleFunc("GET /api/events", s.apiEvents)

	// JSON API
	mux.HandleFunc("GET /api/documents", s.apiSearch)
	mux.HandleFunc("POST /api/documents", s.apiUpload)
	mux.HandleFunc("POST /api/documents/bulk", s.apiBulk)
	mux.HandleFunc("GET /api/documents/{id}", s.apiGetDocument)
	mux.HandleFunc("PATCH /api/documents/{id}", s.apiPatchDocument)
	mux.HandleFunc("DELETE /api/documents/{id}", s.apiDeleteDocument)
	mux.HandleFunc("GET /api/documents/{id}/original", s.serveFile(false, true))
	mux.HandleFunc("GET /api/documents/{id}/archive", s.serveFile(true, true))
	mux.HandleFunc("GET /api/documents/{id}/preview", s.serveFile(true, false))
	mux.HandleFunc("GET /api/documents/{id}/thumb", s.serveThumb)
	mux.HandleFunc("GET /api/documents/{id}/suggestions", s.apiSuggestions)
	mux.HandleFunc("POST /api/documents/{id}/reprocess", s.apiReprocess)
	mux.HandleFunc("POST /api/documents/{id}/ai", s.apiAISuggest)
	mux.HandleFunc("GET /api/tasks", s.apiTasks)
	mux.HandleFunc("GET /api/tasks/{id}", s.apiTask)
	mux.HandleFunc("POST /api/tasks/{id}/unlock", s.apiUnlock)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.apiDiscardTask)
	mux.HandleFunc("GET /api/stats", s.apiStats)
	mux.HandleFunc("POST /api/retrain", s.apiRetrain)
	mux.HandleFunc("GET /api/{kind}", s.apiListEntities)
	mux.HandleFunc("POST /api/{kind}", s.apiCreateEntity)
	mux.HandleFunc("GET /api/{kind}/{id}", s.apiGetEntity)
	mux.HandleFunc("PATCH /api/{kind}/{id}", s.apiUpdateEntity)
	mux.HandleFunc("DELETE /api/{kind}/{id}", s.apiDeleteEntity)

	// HTML interface
	mux.HandleFunc("GET /{$}", s.uiList)
	mux.HandleFunc("GET /inbox", s.uiInbox)
	mux.HandleFunc("POST /bulk", s.uiBulk)
	mux.HandleFunc("GET /documents/{id}", s.uiDocument)
	mux.HandleFunc("GET /documents/{id}/view", s.uiOfficeView)
	mux.HandleFunc("POST /documents/{id}", s.uiSaveDocument)
	mux.HandleFunc("POST /documents/{id}/delete", s.uiDeleteDocument)
	mux.HandleFunc("POST /documents/{id}/reprocess", s.uiReprocess)
	mux.HandleFunc("GET /organize/{kind}", s.uiOrganize)
	mux.HandleFunc("POST /organize/{kind}", s.uiCreateEntity)
	mux.HandleFunc("POST /organize/{kind}/{id}", s.uiUpdateEntity)
	mux.HandleFunc("POST /organize/{kind}/{id}/delete", s.uiDeleteEntity)
	mux.HandleFunc("GET /activity", s.uiActivity)
	mux.HandleFunc("GET /login", s.uiLogin)
	mux.HandleFunc("POST /login", s.uiLoginPost)
	mux.HandleFunc("POST /logout", s.uiLogout)
	static, _ := fs.Sub(assets, "assets")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	return s.logRequests(s.auth(mux))
}

func (s *Server) sessionValue() string {
	h := sha256.Sum256([]byte("paperfull-session:" + s.Token))
	return hex.EncodeToString(h[:])
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	if h := r.Header.Get("Authorization"); h != "" {
		for _, p := range []string{"Bearer ", "Token "} {
			if strings.HasPrefix(h, p) {
				return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, p)), []byte(s.Token)) == 1
			}
		}
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sessionValue())) == 1
	}
	return false
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || strings.HasPrefix(p, "/static/") || s.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			writeError(w, http.StatusUnauthorized, "missing or wrong token (send 'Authorization: Bearer <token>')")
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap lets http.ResponseController reach Flush (needed for live events).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || strings.HasSuffix(r.URL.Path, "/thumb") {
			return
		}
		s.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "took", time.Since(start).Round(time.Millisecond))
	})
}
