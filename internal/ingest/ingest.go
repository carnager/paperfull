// Package ingest runs the consumption pipeline: dedupe, OCR, classify, store.
package ingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"

	"paperfull/internal/classify"
	"paperfull/internal/llm"
	"paperfull/internal/ocr"
	"paperfull/internal/store"
)

type Config struct {
	DataDir        string
	FilenameFormat string
	OCR            ocr.Config
	Workers        int
	DayFirst       bool
	// LLM, if set, is asked for sender, type, title and date of new documents.
	LLM *llm.Client
}

// Options let the caller preset metadata; set fields override the classifier.
type Options struct {
	Title           string
	Created         string
	CorrespondentID *int64
	DocumentTypeID  *int64
	TagIDs          []int64
	Source          string // "folder", "upload", … shown to the user
}

const (
	StatusQueued     = "queued"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusFailed     = "failed"
	StatusDuplicate  = "duplicate"
)

type Task struct {
	ID         string     `json:"id"`
	File       string     `json:"file"`
	Source     string     `json:"source,omitempty"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	DocumentID int64      `json:"document_id,omitempty"`
	Queued     time.Time  `json:"queued"`
	Finished   *time.Time `json:"finished,omitempty"`

	path       string
	opts       Options
	reprocess  int64
	onFinished func(Task)
	done       chan struct{}
}

type Ingester struct {
	cfg   Config
	store *store.Store
	cls   *classify.Classifier
	log   *slog.Logger
	queue chan *Task

	mu    sync.Mutex
	tasks map[string]*Task
	order []string

	fileMu sync.Mutex // guards moves inside the data dir

	// OnTask, if set, is called whenever a task changes state.
	OnTask func(Task)
}

func New(cfg Config, s *store.Store, cls *classify.Classifier, log *slog.Logger) (*Ingester, error) {
	for _, d := range []string{"originals", "archive", "thumbs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Ingester{
		cfg: cfg, store: s, cls: cls, log: log,
		queue: make(chan *Task, 10000),
		tasks: map[string]*Task{},
	}, nil
}

func (in *Ingester) OriginalsDir() string { return filepath.Join(in.cfg.DataDir, "originals") }
func (in *Ingester) ArchiveDir() string   { return filepath.Join(in.cfg.DataDir, "archive") }
func (in *Ingester) TmpDir() string       { return filepath.Join(in.cfg.DataDir, "tmp") }
func (in *Ingester) ThumbPath(id int64) string {
	return filepath.Join(in.cfg.DataDir, "thumbs", fmt.Sprintf("%d.jpg", id))
}

// Run starts the worker pool and blocks until ctx is done.
func (in *Ingester) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < max(in.cfg.Workers, 1); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-in.queue:
					in.runTask(ctx, t)
					// Page images can be large; give the memory back between jobs.
					debug.FreeOSMemory()
				}
			}
		}()
	}
	wg.Wait()
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (in *Ingester) enqueue(t *Task) *Task {
	t.ID, t.Status, t.Queued, t.done = newID(), StatusQueued, time.Now(), make(chan struct{})
	in.mu.Lock()
	in.tasks[t.ID] = t
	in.order = append(in.order, t.ID)
	if len(in.order) > 1000 {
		old := in.order[0]
		in.order = in.order[1:]
		delete(in.tasks, old)
	}
	in.mu.Unlock()
	in.notify(t)
	in.queue <- t
	return t
}

func (in *Ingester) notify(t *Task) {
	if in.OnTask != nil {
		in.OnTask(in.snapshot(t))
	}
}

// Submit queues a file for consumption. onFinished (optional) runs after the
// task ends and is where the caller cleans up the source file.
func (in *Ingester) Submit(path, name string, opts Options, onFinished func(Task)) Task {
	return in.snapshot(in.enqueue(&Task{File: name, Source: opts.Source, path: path, opts: opts, onFinished: onFinished}))
}

// Reprocess re-runs OCR on a stored document, keeping its metadata.
func (in *Ingester) Reprocess(id int64) Task {
	return in.snapshot(in.enqueue(&Task{File: fmt.Sprintf("document #%d", id), Source: "reprocess", reprocess: id}))
}

func (in *Ingester) snapshot(t *Task) Task {
	in.mu.Lock()
	defer in.mu.Unlock()
	c := *t
	return c
}

func (in *Ingester) Task(id string) (Task, bool) {
	in.mu.Lock()
	t, ok := in.tasks[id]
	in.mu.Unlock()
	if !ok {
		return Task{}, false
	}
	return in.snapshot(t), true
}

// Wait blocks until the task finishes or ctx ends.
func (in *Ingester) Wait(ctx context.Context, id string) (Task, bool) {
	in.mu.Lock()
	t, ok := in.tasks[id]
	in.mu.Unlock()
	if !ok {
		return Task{}, false
	}
	select {
	case <-t.done:
	case <-ctx.Done():
	}
	return in.snapshot(t), true
}

// Tasks returns the most recent tasks, newest first.
func (in *Ingester) Tasks(limit int) []Task {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Task
	for i := len(in.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *in.tasks[in.order[i]])
	}
	return out
}

func (in *Ingester) setStatus(t *Task, status string, err error, docID int64) {
	in.mu.Lock()
	t.Status = status
	if err != nil {
		t.Error = err.Error()
	}
	if docID != 0 {
		t.DocumentID = docID
	}
	if status != StatusProcessing {
		now := time.Now()
		t.Finished = &now
	}
	in.mu.Unlock()
	in.notify(t)
}

func (in *Ingester) runTask(ctx context.Context, t *Task) {
	in.setStatus(t, StatusProcessing, nil, 0)
	start := time.Now()
	var id int64
	var err error
	if t.reprocess != 0 {
		id, err = t.reprocess, in.reprocess(ctx, t.reprocess)
	} else {
		id, err = in.consume(ctx, t)
	}
	var dup *DuplicateError
	switch {
	case errors.As(err, &dup):
		in.setStatus(t, StatusDuplicate, err, dup.ExistingID)
		in.log.Warn("duplicate skipped", "file", t.File, "existing", dup.ExistingID)
	case err != nil:
		in.setStatus(t, StatusFailed, err, 0)
		in.log.Error("consume failed", "file", t.File, "err", err)
	default:
		in.setStatus(t, StatusDone, nil, id)
		in.log.Info("document stored", "file", t.File, "id", id, "took", time.Since(start).Round(time.Millisecond))
	}
	if t.onFinished != nil {
		t.onFinished(in.snapshot(t))
	}
	close(t.done)
}

type DuplicateError struct{ ExistingID int64 }

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("duplicate of document #%d", e.ExistingID)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (in *Ingester) consume(ctx context.Context, t *Task) (int64, error) {
	sum, err := sha256File(t.path)
	if err != nil {
		return 0, err
	}
	if id, ok, err := in.store.DocumentByChecksum(ctx, sum); err != nil {
		return 0, err
	} else if ok {
		return 0, &DuplicateError{ExistingID: id}
	}
	mime, err := ocr.DetectMime(t.path)
	if err != nil {
		return 0, err
	}
	if !ocr.Supported(mime) {
		return 0, fmt.Errorf("%w: %s", ocr.ErrUnsupported, mime)
	}

	work, err := os.MkdirTemp(in.TmpDir(), "job-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)

	res, err := ocr.Process(ctx, in.cfg.OCR, t.path, work)
	if err != nil {
		return 0, err
	}
	if res.Warning != "" {
		in.log.Warn(res.Warning)
	}

	pred, err := in.cls.Classify(ctx, res.Text)
	if err != nil {
		return 0, err
	}
	o := t.opts

	// Ask the language model, if configured. It actually reads the letter, so
	// it beats a weak learned guess. Explicit rules and strong learned matches
	// (several similar reviewed documents agreeing) still win. A sender or
	// type that doesn't exist yet is only suggested, so nothing is created
	// behind the user's back.
	var ai llm.Suggestion
	var newSender, newType string
	if in.cfg.LLM != nil {
		var sid, tid *int64
		var aerr error
		ai, sid, tid, aerr = in.AISuggest(ctx, res.Text)
		if aerr != nil {
			in.log.Warn("AI suggestion failed, continuing without", "file", t.File, "err", aerr)
		} else {
			if ai.Sender != "" && o.CorrespondentID == nil && !classify.Strong(pred.CorrespondentFrom) {
				if sid == nil || pred.CorrespondentID == nil || *sid != *pred.CorrespondentID {
					// Different sender than guessed: type and tags have to come
					// from that sender's documents, not the wrong one's.
					if pred, err = in.cls.ClassifyWith(ctx, res.Text, classify.Hint{SenderSet: true, CorrespondentID: sid}); err != nil {
						return 0, err
					}
				}
				if sid == nil {
					newSender = ai.Sender
				}
			}
			if ai.Type != "" && o.DocumentTypeID == nil && !classify.Strong(pred.DocumentTypeFrom) {
				pred.DocumentTypeID = tid
				if tid == nil {
					newType = ai.Type
				}
			}
		}
	}
	if o.CorrespondentID != nil {
		pred.CorrespondentID, newSender = o.CorrespondentID, ""
	}
	if o.DocumentTypeID != nil {
		pred.DocumentTypeID, newType = o.DocumentTypeID, ""
	}
	tags := append([]int64{}, pred.TagIDs...)
	tags = append(tags, o.TagIDs...)

	created := o.Created
	if created == "" {
		created = ai.Date
	}
	if created == "" {
		created = classify.FindDate(res.Text, in.cfg.DayFirst)
	}
	if created == "" {
		created = time.Now().Format("2006-01-02")
	}
	title := strings.TrimSpace(o.Title)
	if title == "" && ai.Title != "" && isJunkName(t.File) {
		title = ai.Title
	}
	if title == "" {
		title = in.guessTitle(ctx, t.File, res.Text, pred)
	}

	id, err := in.store.InsertDocument(ctx, store.NewDocument{
		Title: title, Content: res.Text, Created: created, Checksum: sum,
		OriginalName: t.File, Mime: res.Mime, Pages: res.Pages, Inbox: true,
		CorrespondentID: pred.CorrespondentID, DocumentTypeID: pred.DocumentTypeID, TagIDs: tags,
		SuggestedSender: newSender, SuggestedType: newType,
	})
	if err != nil {
		return 0, err
	}
	if err := in.placeNew(ctx, id, t.path, res.ArchivePath); err != nil {
		in.store.DeleteDocument(context.Background(), id)
		return 0, fmt.Errorf("storing files: %w", err)
	}
	return id, nil
}

func (in *Ingester) placeNew(ctx context.Context, id int64, src, archive string) error {
	d, err := in.store.GetDocument(ctx, id)
	if err != nil {
		return err
	}
	in.fileMu.Lock()
	defer in.fileMu.Unlock()
	base := in.renderPath(d)
	ext := strings.ToLower(filepath.Ext(d.OriginalName))
	if ext == "" || len(ext) > 6 {
		ext = ocr.ExtFor(d.Mime)
	}
	rel := uniquePath(in.OriginalsDir(), base, ext, "")
	if err := copyFile(src, filepath.Join(in.OriginalsDir(), rel)); err != nil {
		return err
	}
	arel := ""
	if archive != "" {
		arel = uniquePath(in.ArchiveDir(), base, ".pdf", "")
		if err := moveFile(archive, filepath.Join(in.ArchiveDir(), arel)); err != nil {
			os.Remove(filepath.Join(in.OriginalsDir(), rel))
			return err
		}
	}
	if err := in.store.SetPaths(ctx, id, rel, arel); err != nil {
		return err
	}
	in.makeThumb(ctx, id, rel, arel, d.Mime)
	return nil
}

func (in *Ingester) makeThumb(ctx context.Context, id int64, rel, arel, mime string) {
	pdf := ""
	switch {
	case arel != "":
		pdf = filepath.Join(in.ArchiveDir(), arel)
	case mime == "application/pdf":
		pdf = filepath.Join(in.OriginalsDir(), rel)
	default:
		os.Remove(in.ThumbPath(id))
		return
	}
	if err := ocr.Thumbnail(ctx, pdf, in.ThumbPath(id)); err != nil {
		in.log.Warn("thumbnail failed", "id", id, "err", err)
	}
}

func (in *Ingester) reprocess(ctx context.Context, id int64) error {
	d, err := in.store.GetDocument(ctx, id)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(in.TmpDir(), "job-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	res, err := ocr.Process(ctx, in.cfg.OCR, filepath.Join(in.OriginalsDir(), d.Path), work)
	if err != nil {
		return err
	}
	if res.Warning != "" {
		in.log.Warn(res.Warning)
	}
	in.fileMu.Lock()
	defer in.fileMu.Unlock()
	arel := d.ArchivePath
	if res.ArchivePath != "" {
		if arel == "" {
			arel = uniquePath(in.ArchiveDir(), in.renderPath(d), ".pdf", "")
		}
		if err := moveFile(res.ArchivePath, filepath.Join(in.ArchiveDir(), arel)); err != nil {
			return err
		}
	} else if arel != "" {
		removeAndPrune(in.ArchiveDir(), arel)
		arel = ""
	}
	if err := in.store.SetContent(ctx, id, res.Text, res.Pages, arel); err != nil {
		return err
	}
	in.makeThumb(ctx, id, d.Path, arel, d.Mime)
	return nil
}

// Relocate moves a document's files to match the filename format after its
// metadata changed.
func (in *Ingester) Relocate(ctx context.Context, id int64) error {
	d, err := in.store.GetDocument(ctx, id)
	if err != nil {
		return err
	}
	in.fileMu.Lock()
	defer in.fileMu.Unlock()
	base := in.renderPath(d)
	rel := uniquePath(in.OriginalsDir(), base, filepath.Ext(d.Path), d.Path)
	if rel != d.Path {
		if err := moveFile(filepath.Join(in.OriginalsDir(), d.Path), filepath.Join(in.OriginalsDir(), rel)); err != nil {
			return err
		}
		pruneDirs(in.OriginalsDir(), filepath.Dir(d.Path))
	}
	arel := d.ArchivePath
	if arel != "" {
		arel = uniquePath(in.ArchiveDir(), base, ".pdf", d.ArchivePath)
		if arel != d.ArchivePath {
			if err := moveFile(filepath.Join(in.ArchiveDir(), d.ArchivePath), filepath.Join(in.ArchiveDir(), arel)); err != nil {
				return err
			}
			pruneDirs(in.ArchiveDir(), filepath.Dir(d.ArchivePath))
		}
	}
	if rel == d.Path && arel == d.ArchivePath {
		return nil
	}
	return in.store.SetPaths(ctx, id, rel, arel)
}

// Delete removes a document and its files.
func (in *Ingester) Delete(ctx context.Context, id int64) error {
	d, err := in.store.DeleteDocument(ctx, id)
	if err != nil {
		return err
	}
	in.fileMu.Lock()
	defer in.fileMu.Unlock()
	if d.Path != "" {
		removeAndPrune(in.OriginalsDir(), d.Path)
	}
	if d.ArchivePath != "" {
		removeAndPrune(in.ArchiveDir(), d.ArchivePath)
	}
	os.Remove(in.ThumbPath(id))
	return nil
}

// isJunkName reports whether a file name says nothing, like scan_0001.pdf.
func isJunkName(file string) bool {
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	return junkName.MatchString(strings.TrimSpace(strings.ReplaceAll(name, "_", " ")))
}

// AIEnabled reports whether a language model is configured.
func (in *Ingester) AIEnabled() bool { return in.cfg.LLM != nil }

// AISuggest asks the language model about a document and maps its answer
// onto existing senders and types. senderID/typeID are set when the
// suggestion matches something that already exists; the names in s are
// then replaced by the existing spelling.
func (in *Ingester) AISuggest(ctx context.Context, text string) (s llm.Suggestion, senderID, typeID *int64, err error) {
	if in.cfg.LLM == nil {
		return s, nil, nil, errors.New("no AI model configured")
	}
	names := func(k store.Kind) []string {
		es, _ := in.store.ListEntities(ctx, k)
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = e.Name
		}
		return out
	}
	start := time.Now()
	s, err = in.cfg.LLM.Suggest(ctx, text, names(store.KindCorrespondent))
	if err != nil {
		return s, nil, nil, err
	}
	in.log.Info("AI suggestion", "sender", s.Sender, "type", s.Type, "title", s.Title, "date", s.Date, "took", time.Since(start).Round(time.Millisecond))
	if s.Sender != "" {
		if e, ok := in.store.FindSimilarEntity(ctx, store.KindCorrespondent, s.Sender); ok {
			s.Sender, senderID = e.Name, &e.ID
		}
	}
	if s.Type != "" {
		if e, ok := in.store.FindSimilarType(ctx, s.Type); ok {
			s.Type, typeID = e.Name, &e.ID
		}
	}
	return s, senderID, typeID, nil
}

var junkName = regexp.MustCompile(`(?i)^(scan|scanned|img|image|dsc|pxl|doc|document|dokument|file|untitled|page|seite|brn[0-9a-f]+)?[\s_\-.]*[\d\s_\-.]*$`)

func (in *Ingester) guessTitle(ctx context.Context, file, text string, p classify.Prediction) string {
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	name = strings.TrimSpace(strings.NewReplacer("_", " ").Replace(name))
	if name != "" && !junkName.MatchString(name) {
		return name
	}
	var parts []string
	if p.DocumentTypeID != nil {
		if e, err := in.store.GetEntity(ctx, store.KindDocumentType, *p.DocumentTypeID); err == nil {
			parts = append(parts, e.Name)
		}
	}
	if p.CorrespondentID != nil {
		if e, err := in.store.GetEntity(ctx, store.KindCorrespondent, *p.CorrespondentID); err == nil {
			parts = append(parts, e.Name)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		letters := 0
		for _, r := range line {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters >= 4 {
			if r := []rune(line); len(r) > 80 {
				line = string(r[:80])
			}
			return line
		}
	}
	if name != "" {
		return name
	}
	return "Untitled"
}
