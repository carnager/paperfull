package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"paperfull/internal/ocr"
)

// Password-protected PDFs can't be read until someone types the password.
// They wait in data/locked/ (a copy of the file plus a small JSON note) so
// the source can be cleaned up and nothing is lost across restarts.

const (
	StatusPassword  = "password"  // waiting for its password
	StatusUnlocked  = "unlocked"  // password given, processing continues as a new task
	StatusDiscarded = "discarded" // the user gave up on it
)

var ErrNotLocked = errors.New("this task isn't waiting for a password")

// LockedError means the document was set aside until its password is given.
type LockedError struct{}

func (LockedError) Error() string { return ocr.ErrPasswordRequired.Error() }

type lockedNote struct {
	File     string    `json:"file"`
	Source   string    `json:"source"`
	Checksum string    `json:"checksum"`
	Options  Options   `json:"options"`
	Queued   time.Time `json:"queued"`
}

func (in *Ingester) LockedDir() string { return filepath.Join(in.cfg.DataDir, "locked") }

func (in *Ingester) lockedPaths(id string) (pdf, note string) {
	return filepath.Join(in.LockedDir(), id+".pdf"), filepath.Join(in.LockedDir(), id+".json")
}

// park keeps a copy of a locked PDF and its options until it is unlocked.
func (in *Ingester) park(t *Task, checksum string) error {
	in.mu.Lock()
	for _, o := range in.tasks {
		if o != t && o.Status == StatusPassword && o.checksum == checksum {
			in.mu.Unlock()
			return errors.New("the same file is already waiting for its password")
		}
	}
	in.mu.Unlock()
	pdf, note := in.lockedPaths(t.ID)
	if err := copyFile(t.path, pdf); err != nil {
		return fmt.Errorf("keeping the locked PDF: %w", err)
	}
	b, _ := json.Marshal(lockedNote{File: t.File, Source: t.Source, Checksum: checksum, Options: t.opts, Queued: t.Queued})
	if err := os.WriteFile(note, b, 0o644); err != nil {
		os.Remove(pdf)
		return err
	}
	in.mu.Lock()
	t.checksum = checksum
	in.mu.Unlock()
	return LockedError{}
}

// restoreLocked brings back documents that were waiting for a password
// when paperfull last stopped.
func (in *Ingester) restoreLocked() {
	notes, _ := filepath.Glob(filepath.Join(in.LockedDir(), "*.json"))
	sort.Strings(notes)
	for _, note := range notes {
		id := strings.TrimSuffix(filepath.Base(note), ".json")
		b, err := os.ReadFile(note)
		var n lockedNote
		if err != nil || json.Unmarshal(b, &n) != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(in.LockedDir(), id+".pdf")); err != nil {
			os.Remove(note)
			continue
		}
		t := &Task{ID: id, File: n.File, Source: n.Source, Status: StatusPassword, Queued: n.Queued,
			Error: ocr.ErrPasswordRequired.Error(), opts: n.Options, checksum: n.Checksum, done: make(chan struct{})}
		close(t.done)
		in.mu.Lock()
		in.tasks[id] = t
		in.order = append(in.order, id)
		in.mu.Unlock()
	}
	if len(notes) > 0 {
		in.log.Info("documents waiting for a password", "count", len(notes))
	}
}

// LockedCount is how many documents wait for a password.
func (in *Ingester) LockedCount() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for _, t := range in.tasks {
		if t.Status == StatusPassword {
			n++
		}
	}
	return n
}

func (in *Ingester) lockedTask(id string) (*Task, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	t, ok := in.tasks[id]
	if !ok || t.Status != StatusPassword {
		return nil, ErrNotLocked
	}
	return t, nil
}

// Unlock decrypts a waiting PDF and queues it for normal processing. It
// returns the new task. The stored original is the unlocked copy, so
// previews and searching work without asking again.
func (in *Ingester) Unlock(ctx context.Context, id, password string) (Task, error) {
	t, err := in.lockedTask(id)
	if err != nil {
		return Task{}, err
	}
	pdf, note := in.lockedPaths(id)
	out := filepath.Join(in.TmpDir(), "unlocked-"+id+".pdf")
	if err := ocr.DecryptPDF(ctx, pdf, out, password); err != nil {
		return Task{}, err
	}
	os.Remove(pdf)
	os.Remove(note)
	in.setStatus(t, StatusUnlocked, nil, 0)
	in.log.Info("PDF unlocked", "file", t.File)
	opts := t.opts
	opts.Source = t.Source
	return in.Submit(out, t.File, opts, func(Task) { os.Remove(out) }), nil
}

// Discard drops a document that was waiting for its password.
func (in *Ingester) Discard(id string) error {
	t, err := in.lockedTask(id)
	if err != nil {
		return err
	}
	pdf, note := in.lockedPaths(id)
	os.Remove(pdf)
	os.Remove(note)
	in.setStatus(t, StatusDiscarded, nil, 0)
	return nil
}
