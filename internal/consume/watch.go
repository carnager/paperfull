// Package consume watches a folder and feeds new files to the ingester.
package consume

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"paperfull/internal/ingest"
	"paperfull/internal/store"
)

type Watcher struct {
	Dir        string
	SubdirTags bool          // turn sub-folder names into tags
	Settle     time.Duration // how long a file must stay unchanged before it is taken
	Ingester   *ingest.Ingester
	Store      *store.Store
	Log        *slog.Logger

	mu       sync.Mutex
	pending  map[string]*pendingFile
	inflight map[string]bool
}

type pendingFile struct {
	size    int64
	mod     time.Time
	changed time.Time
}

func (w *Watcher) Run(ctx context.Context) error {
	w.pending = map[string]*pendingFile{}
	w.inflight = map[string]bool{}
	if w.Settle == 0 {
		w.Settle = 3 * time.Second
	}
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()
	w.addTree(fsw, w.Dir)
	w.scan()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// Periodic full scans catch files on network shares where inotify is silent.
	rescan := time.NewTicker(30 * time.Second)
	defer rescan.Stop()
	w.Log.Info("watching consume folder", "dir", w.Dir)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Chmod) == 0 {
				continue
			}
			if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
				if !ignored(ev.Name) {
					w.addTree(fsw, ev.Name)
				}
				continue
			}
			w.note(ev.Name)
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.Log.Warn("watcher error", "err", err)
		case <-tick.C:
			w.check()
		case <-rescan.C:
			w.scan()
		}
	}
}

func ignored(path string) bool {
	b := filepath.Base(path)
	if strings.HasPrefix(b, ".") || strings.HasPrefix(b, "~") || strings.HasSuffix(b, "~") {
		return true
	}
	switch strings.ToLower(filepath.Ext(b)) {
	case ".part", ".crdownload", ".tmp", ".swp", ".partial", ".err":
		return true
	}
	return false
}

func (w *Watcher) addTree(fsw *fsnotify.Watcher, root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != w.Dir && ignored(p) {
				return filepath.SkipDir
			}
			if err := fsw.Add(p); err != nil {
				w.Log.Warn("cannot watch", "dir", p, "err", err)
			}
		} else {
			w.note(p)
		}
		return nil
	})
}

func (w *Watcher) scan() {
	filepath.WalkDir(w.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != w.Dir && ignored(p) {
				return filepath.SkipDir
			}
			return nil
		}
		w.note(p)
		return nil
	})
}

func (w *Watcher) note(path string) {
	if ignored(path) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inflight[path] {
		return
	}
	if _, ok := w.pending[path]; !ok {
		w.pending[path] = &pendingFile{size: -1, changed: time.Now()}
	}
}

// check hands files to the ingester once their size and mtime stopped changing.
func (w *Watcher) check() {
	now := time.Now()
	var ready []string
	w.mu.Lock()
	for p, pf := range w.pending {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			delete(w.pending, p)
			continue
		}
		if fi.Size() != pf.size || !fi.ModTime().Equal(pf.mod) {
			pf.size, pf.mod, pf.changed = fi.Size(), fi.ModTime(), now
			continue
		}
		if fi.Size() > 0 && now.Sub(pf.changed) >= w.Settle {
			delete(w.pending, p)
			w.inflight[p] = true
			ready = append(ready, p)
		}
	}
	w.mu.Unlock()
	for _, p := range ready {
		w.submit(p)
	}
}

func (w *Watcher) submit(path string) {
	opts := ingest.Options{Source: "folder"}
	if w.SubdirTags {
		rel, err := filepath.Rel(w.Dir, filepath.Dir(path))
		if err == nil && rel != "." {
			for _, name := range strings.Split(rel, string(filepath.Separator)) {
				id, err := w.Store.EnsureEntity(context.Background(), store.KindTag, name)
				if err != nil {
					w.Log.Warn("creating folder tag", "tag", name, "err", err)
					continue
				}
				opts.TagIDs = append(opts.TagIDs, id)
			}
		}
	}
	w.Log.Info("consuming", "file", path)
	w.Ingester.Submit(path, filepath.Base(path), opts, func(t ingest.Task) {
		defer func() {
			w.mu.Lock()
			delete(w.inflight, path)
			w.mu.Unlock()
		}()
		if t.Status == ingest.StatusDone {
			if err := os.Remove(path); err != nil {
				w.Log.Warn("removing consumed file", "file", path, "err", err)
			}
			return
		}
		w.moveFailed(path, t.Error)
	})
}

// moveFailed puts a file that could not be consumed into .failed/ with a
// note explaining why, so it is not retried forever.
func (w *Watcher) moveFailed(path, reason string) {
	dir := filepath.Join(w.Dir, ".failed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.Log.Error("creating .failed", "err", err)
		return
	}
	name := filepath.Base(path)
	dst := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(name)
		dst = filepath.Join(dir, strings.TrimSuffix(name, ext)+"-"+strconv.Itoa(i)+ext)
	}
	if err := os.Rename(path, dst); err != nil {
		w.Log.Error("moving failed file", "file", path, "err", err)
		return
	}
	os.WriteFile(dst+".err", []byte(reason+"\n"), 0o644)
	w.Log.Warn("moved to .failed", "file", dst, "reason", reason)
}
