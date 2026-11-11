package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"paperfull/internal/classify"
	"paperfull/internal/ingest"
	"paperfull/internal/store"
)

func newServer(t *testing.T) (*Server, http.Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cls := classify.New(st, log)
	in, err := ingest.New(ingest.Config{DataDir: filepath.Join(dir, "data"), Workers: 1}, st, cls, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go in.Run(ctx)
	s := &Server{Store: st, Ingester: in, Cls: cls, Log: log}
	return s, s.Handler(), st
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// Every page renders, with and without documents.
func TestPagesRender(t *testing.T) {
	s, h, st := newServer(t)
	pages := []string{"/", "/inbox", "/activity", "/organize/tags", "/organize/correspondents", "/organize/document_types",
		"/?q=tag:none+after:2024", "/?q=after:bad", "/?text=strom&f=from:x", "/login"}
	check := func() {
		for _, p := range pages {
			code, body := get(t, h, p)
			if code != 200 || strings.Contains(body, "template error") {
				t.Errorf("%s: %d %s", p, code, body[:min(len(body), 300)])
			}
		}
	}
	check()

	// Add a document through the API and check the pages again.
	f := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(f, []byte("Stadtwerke Musterstadt\nRechnung vom 11.03.2026"), 0o644)
	task := s.Ingester.Submit(f, "note.txt", ingest.Options{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, _ = s.Ingester.Wait(ctx, task.ID)
	if task.Status != ingest.StatusDone {
		t.Fatalf("adding a document: %s %s", task.Status, task.Error)
	}
	st.CreateEntity(context.Background(), store.KindTag, store.Entity{Name: "Strom", Color: "#46a758"})
	pages = append(pages, "/documents/1", "/api/documents/1/thumb", "/api/documents", "/api/documents/1", "/?q=strom&partial=1")
	check()
	if code, _ := get(t, h, "/documents/999"); code != 404 {
		t.Errorf("missing document: %d", code)
	}
}

func TestAuth(t *testing.T) {
	s, h, _ := newServer(t)
	s.Token = "secret"
	if code, _ := get(t, h, "/api/documents"); code != 401 {
		t.Errorf("API without token: %d", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("UI without login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	req := httptest.NewRequest(http.MethodGet, "/api/documents", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("API with token: %d", rec.Code)
	}
}

func TestUnlockEndpoint(t *testing.T) {
	s, h, _ := newServer(t)
	p := filepath.Join(t.TempDir(), "locked.pdf")
	if err := api.EncryptFile(context.Background(), "../../samples/Stromrechnung-2025.pdf", p, model.NewAESConfiguration("geheim", "x", 256)); err != nil {
		t.Fatal(err)
	}
	task := s.Ingester.Submit(p, "locked.pdf", ingest.Options{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, _ = s.Ingester.Wait(ctx, task.ID)
	if task.Status != ingest.StatusPassword {
		t.Fatalf("status %s", task.Status)
	}
	post := func(body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/unlock", strings.NewReader(body)))
		return rec.Code
	}
	if code := post(`{"password":"falsch"}`); code != http.StatusForbidden {
		t.Errorf("wrong password: %d", code)
	}
	if code, body := get(t, h, "/activity"); code != 200 || !strings.Contains(body, `data-unlock="`+task.ID+`"`) {
		t.Errorf("activity page should offer to unlock (%d)", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+task.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("discard: %d", rec.Code)
	}
	if code := post(`{"password":"geheim"}`); code != http.StatusConflict {
		t.Errorf("unlock after discard: %d", code)
	}
}
