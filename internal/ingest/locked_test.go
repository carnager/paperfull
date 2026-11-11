package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"paperfull/internal/ocr"
)

func lockedSample(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "kontoauszug.pdf")
	err := api.EncryptFile(context.Background(), "../../samples/Stromrechnung-2025.pdf", p, model.NewAESConfiguration("geheim", "bank-only", 256))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) submitAndWait(p string) Task {
	e.t.Helper()
	task := e.in.Submit(p, filepath.Base(p), Options{Source: "folder"}, nil)
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	task, _ = e.in.Wait(ctx, task.ID)
	return task
}

func TestLockedPDFWaitsAndSurvivesRestart(t *testing.T) {
	e := newEnv(t, nil)
	locked := lockedSample(t, e.dir)
	task := e.submitAndWait(locked)
	if task.Status != StatusPassword {
		t.Fatalf("status %q (%s), want %q", task.Status, task.Error, StatusPassword)
	}
	if n := e.in.LockedCount(); n != 1 {
		t.Errorf("LockedCount = %d", n)
	}
	// The same file again shouldn't wait twice.
	if again := e.submitAndWait(locked); again.Status != StatusFailed {
		t.Errorf("second copy: status %q, want failed", again.Status)
	}

	// A new Ingester on the same data folder (a restart) still knows it.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	in2, err := New(e.in.cfg, e.st, e.cls, log)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := in2.Task(task.ID)
	if !ok || restored.Status != StatusPassword || restored.File != "kontoauszug.pdf" || restored.Source != "folder" {
		t.Fatalf("not restored after restart: %+v", restored)
	}

	if _, err := in2.Unlock(e.ctx, task.ID, "falsch"); !errors.Is(err, ocr.ErrWrongPassword) {
		t.Errorf("wrong password: %v", err)
	}
	if err := in2.Discard(task.ID); err != nil {
		t.Fatal(err)
	}
	if files, _ := os.ReadDir(in2.LockedDir()); len(files) != 0 {
		t.Errorf("discard left files behind: %d", len(files))
	}
	if _, err := in2.Unlock(e.ctx, task.ID, "geheim"); !errors.Is(err, ErrNotLocked) {
		t.Errorf("unlocking a discarded task: %v", err)
	}
}

func TestUnlockStoresDocument(t *testing.T) {
	if err := ocr.CheckTools(); err != nil {
		t.Skip(err)
	}
	e := newEnv(t, nil)
	// The sample is a scan, so this one really runs OCR.
	e.in.cfg.OCR = ocr.Config{Languages: "eng", DPI: 150, Jobs: 2, PSM: 3}
	task := e.submitAndWait(lockedSample(t, e.dir))
	next, err := e.in.Unlock(e.ctx, task.ID, "geheim")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	next, _ = e.in.Wait(ctx, next.ID)
	if next.Status != StatusDone {
		t.Fatalf("after unlocking: %s %s", next.Status, next.Error)
	}
	old, _ := e.in.Task(task.ID)
	if old.Status != StatusUnlocked {
		t.Errorf("old task status %q", old.Status)
	}
	d, err := e.st.GetDocument(e.ctx, next.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if d.OriginalName != "kontoauszug.pdf" || d.Created != "2026-03-11" {
		t.Errorf("stored as %q dated %s", d.OriginalName, d.Created)
	}
	if ocr.NeedsPassword(e.ctx, filepath.Join(e.in.OriginalsDir(), d.Path)) {
		t.Error("the stored original should be the unlocked copy")
	}
}
