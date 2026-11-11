package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paperfull/internal/classify"
	"paperfull/internal/llm"
	"paperfull/internal/store"
)

// These tests run the whole intake (classify, AI, store) on plain text
// files, which need no OCR tools, with a fake Ollama.

type env struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	cls   *classify.Classifier
	in    *Ingester
	dir   string
	calls int
}

// aiAnswers maps a word in the document to the fake model's answer.
type aiAnswers map[string]llm.Suggestion

func newEnv(t *testing.T, answers aiAnswers) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.ctx = ctx
	var err error
	if e.st, err = store.Open(filepath.Join(e.dir, "db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.cls = classify.New(e.st, log)
	cfg := Config{DataDir: filepath.Join(e.dir, "data"), Workers: 1, DayFirst: true}
	if answers != nil {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e.calls++
			var req struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			text := req.Messages[len(req.Messages)-1].Content
			var s llm.Suggestion
			for word, a := range answers {
				if strings.Contains(text, word) {
					s = a
				}
			}
			b, _ := json.Marshal(s)
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(b)}})
		}))
		t.Cleanup(srv.Close)
		cfg.LLM = llm.New(srv.URL, "fake")
	}
	if e.in, err = New(cfg, e.st, e.cls, log); err != nil {
		t.Fatal(err)
	}
	go e.in.Run(ctx)
	return e
}

// add consumes a text file and returns the stored document.
func (e *env) add(name, text string) *store.Document {
	e.t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	task := e.in.Submit(p, name, Options{}, nil)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	task, _ = e.in.Wait(ctx, task.ID)
	if task.Status != StatusDone {
		e.t.Fatalf("%s: status %s: %s", name, task.Status, task.Error)
	}
	d, err := e.st.GetDocument(e.ctx, task.DocumentID)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// review files a document the way a user pressing Done would, then retrains.
func (e *env) review(d *store.Document, sender, typ string, tags ...string) {
	e.t.Helper()
	sid, _ := e.st.EnsureEntity(e.ctx, store.KindCorrespondent, sender)
	tid, _ := e.st.EnsureEntity(e.ctx, store.KindDocumentType, typ)
	var tagIDs []int64
	for _, n := range tags {
		id, _ := e.st.EnsureEntity(e.ctx, store.KindTag, n)
		tagIDs = append(tagIDs, id)
	}
	f := false
	err := e.st.UpdateDocuments(e.ctx, []int64{d.ID}, store.DocUpdate{
		Correspondent: &store.NullID{ID: sid, Valid: true}, DocumentType: &store.NullID{ID: tid, Valid: true},
		Tags: &tagIDs, Inbox: &f,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.cls.Train(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func name(r *store.Ref) string {
	if r == nil {
		return ""
	}
	return r.Name
}

func tagNames(d *store.Document) []string {
	var out []string
	for _, t := range d.Tags {
		out = append(out, t.Name)
	}
	return out
}

const (
	stromBill = `Stadtwerke Musterstadt GmbH · Energiestraße 12 · 12345 Musterstadt
Herrn Max Mustermann, Lindenweg 7, 12345 Musterstadt
Kundennummer 4711 Rechnungsdatum: 11.03.2026
Jahresrechnung Strom 2025. Verbrauch 3.214 kWh, Arbeitspreis, Grundpreis, Abschläge.`
	stromBill2 = `Stadtwerke Musterstadt GmbH · Energiestraße 12 · 12345 Musterstadt
Herrn Max Mustermann, Lindenweg 7, 12345 Musterstadt
Kundennummer 4711 Rechnungsdatum: 10.03.2025
Jahresrechnung Strom 2024. Verbrauch 3.502 kWh, Arbeitspreis, Grundpreis, Abschläge.`
	insurance = `Sicherheit Plus Versicherung AG · Postfach 10 20 30 · 12345 Musterstadt
Herrn Max Mustermann, Lindenweg 7, 12345 Musterstadt
Datum: 15.10.2026. Beitragsanpassung Ihrer Hausratversicherung zum 1. Januar 2027.`
)

var ai = aiAnswers{
	"Stadtwerke":       {Sender: "Stadtwerke Musterstadt GmbH", Type: "Stromrechnung", Title: "Jahresrechnung Strom", Date: "2026-03-11"},
	"Sicherheit Plus":  {Sender: "Sicherheit Plus Versicherung AG", Type: "Brief", Title: "Beitragsanpassung Hausrat", Date: "2026-10-15"},
	"Nothing to learn": {},
}

func TestAINewSenderIsOnlySuggested(t *testing.T) {
	e := newEnv(t, ai)
	d := e.add("scan_0001.txt", insurance)
	if d.Correspondent != nil || d.SuggestedSender != "Sicherheit Plus Versicherung AG" {
		t.Errorf("new sender should be suggested, not created: got %q / suggested %q", name(d.Correspondent), d.SuggestedSender)
	}
	if d.DocumentType != nil || d.SuggestedType != "Brief" {
		t.Errorf("new type should be suggested: got %q / suggested %q", name(d.DocumentType), d.SuggestedType)
	}
	if d.Title != "Beitragsanpassung Hausrat" {
		t.Errorf("a junk file name should get the AI title, got %q", d.Title)
	}
	if d.Created != "2026-10-15" {
		t.Errorf("date: got %s", d.Created)
	}
	senders, _ := e.st.ListEntities(e.ctx, store.KindCorrespondent)
	if len(senders) != 0 {
		t.Errorf("nothing should be created behind the user's back, got %v", senders)
	}
}

func TestAIMatchesExistingNames(t *testing.T) {
	e := newEnv(t, ai)
	e.st.CreateEntity(e.ctx, store.KindCorrespondent, store.Entity{Name: "Stadtwerke Musterstadt", Algorithm: "auto"})
	e.st.CreateEntity(e.ctx, store.KindDocumentType, store.Entity{Name: "Rechnung", Algorithm: "auto"})
	d := e.add("Strom 2025.txt", stromBill)
	if name(d.Correspondent) != "Stadtwerke Musterstadt" || d.SuggestedSender != "" {
		t.Errorf("'… GmbH' should land on the existing sender, got %q (suggested %q)", name(d.Correspondent), d.SuggestedSender)
	}
	if name(d.DocumentType) != "Rechnung" {
		t.Errorf("'Stromrechnung' should land on the existing type 'Rechnung', got %q", name(d.DocumentType))
	}
	if d.Title != "Strom 2025" {
		t.Errorf("a meaningful file name should be kept, got %q", d.Title)
	}
}

// The bug from real use: one reviewed Stadtwerke bill, then an insurance
// letter. Both carry the user's address; the learned guess must not beat
// the AI, and type and tags must not be copied from the Stadtwerke bill.
func TestAIBeatsWeakLearnedGuess(t *testing.T) {
	e := newEnv(t, ai)
	e.review(e.add("bill.txt", stromBill), "Stadtwerke Musterstadt", "Rechnung", "Strom")
	d := e.add("letter.txt", insurance)
	if d.Correspondent != nil || d.SuggestedSender != "Sicherheit Plus Versicherung AG" {
		t.Errorf("sender: got %q (suggested %q)", name(d.Correspondent), d.SuggestedSender)
	}
	if name(d.DocumentType) == "Rechnung" || d.SuggestedType != "Brief" {
		t.Errorf("type: got %q (suggested %q)", name(d.DocumentType), d.SuggestedType)
	}
	if len(d.Tags) != 0 {
		t.Errorf("tags from another sender's documents: %v", tagNames(d))
	}
}

func TestStrongLearnedGuessBeatsAI(t *testing.T) {
	// The model gets this one wrong; two reviewed bills that agree win.
	e := newEnv(t, aiAnswers{"Stadtwerke": {Sender: "Energieversorger Nord", Type: "Brief"}})
	e.review(e.add("a.txt", stromBill), "Stadtwerke Musterstadt", "Rechnung", "Strom")
	e.review(e.add("b.txt", stromBill2), "Stadtwerke Musterstadt", "Rechnung", "Strom")
	d := e.add("c.txt", strings.ReplaceAll(stromBill, "2025", "2026"))
	if name(d.Correspondent) != "Stadtwerke Musterstadt" || name(d.DocumentType) != "Rechnung" {
		t.Errorf("got %q / %q", name(d.Correspondent), name(d.DocumentType))
	}
	if strings.Join(tagNames(d), ",") != "Strom" {
		t.Errorf("tags: %v", tagNames(d))
	}
}

func TestRulesBeatAI(t *testing.T) {
	e := newEnv(t, ai)
	e.st.CreateEntity(e.ctx, store.KindCorrespondent, store.Entity{Name: "Meine Versicherung", Algorithm: "any", Match: "Hausratversicherung"})
	d := e.add("letter.txt", insurance)
	if name(d.Correspondent) != "Meine Versicherung" || d.SuggestedSender != "" {
		t.Errorf("rule should win: got %q (suggested %q)", name(d.Correspondent), d.SuggestedSender)
	}
}

func TestWithoutAIStillWorks(t *testing.T) {
	e := newEnv(t, nil)
	d := e.add("scan_0001.txt", insurance)
	if d.Created != "2026-10-15" {
		t.Errorf("date from the text: got %s", d.Created)
	}
	if d.SuggestedSender != "" || d.Correspondent != nil {
		t.Errorf("no AI, nothing learned: got %q / %q", name(d.Correspondent), d.SuggestedSender)
	}
}

func TestAIDownDoesNotBlock(t *testing.T) {
	e := newEnv(t, nil)
	e.in.cfg.LLM = llm.New("http://127.0.0.1:1", "fake")
	d := e.add("letter.txt", insurance)
	if d.ID == 0 || d.Created != "2026-10-15" {
		t.Errorf("document should be stored normally, got %+v", d)
	}
}
