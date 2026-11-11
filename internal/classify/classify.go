// Package classify assigns correspondents, document types and tags to new
// documents, using explicit match rules and a nearest-neighbour model built
// from documents the user has already reviewed.
package classify

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"paperfull/internal/store"
)

// Prediction is what the classifier suggests for a document.
type Prediction struct {
	CorrespondentID *int64  `json:"correspondent_id"`
	DocumentTypeID  *int64  `json:"document_type_id"`
	TagIDs          []int64 `json:"tag_ids"`
	// Where the guesses came from, see From*.
	CorrespondentFrom string `json:"correspondent_from,omitempty"`
	DocumentTypeFrom  string `json:"document_type_from,omitempty"`
}

// Sources of a guess. Rules and strong learned matches outrank AI
// suggestions; a weak learned match (one or two vaguely similar documents)
// does not.
const (
	FromRule    = "rule"
	FromLearned = "learned"      // several similar reviewed documents agree
	FromWeak    = "learned-weak" // only a loose resemblance
	FromGiven   = "given"        // decided by the caller, e.g. the AI
)

// Strong reports whether a guess should be kept over an AI suggestion.
func Strong(from string) bool { return from == FromRule || from == FromLearned }

// Hint lets the caller decide the sender before type and tags are guessed.
type Hint struct {
	SenderSet       bool
	CorrespondentID *int64 // nil with SenderSet: a sender that doesn't exist yet
}

type Classifier struct {
	store *store.Store
	log   *slog.Logger
	model atomic.Pointer[model]
	dirty atomic.Bool
	mu    sync.Mutex // serializes training
}

func New(s *store.Store, log *slog.Logger) *Classifier {
	return &Classifier{store: s, log: log}
}

// MarkDirty schedules a retrain on the next tick of Run.
func (c *Classifier) MarkDirty() { c.dirty.Store(true) }

// Run retrains on start and then whenever documents changed.
func (c *Classifier) Run(ctx context.Context, every time.Duration) {
	if err := c.Train(ctx); err != nil {
		c.log.Error("training classifier", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.dirty.Swap(false) {
				if err := c.Train(ctx); err != nil {
					c.log.Error("training classifier", "err", err)
				}
			}
		}
	}
}

func (c *Classifier) Train(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := time.Now()
	m := newModel()
	err := c.store.EachTrainingDoc(ctx, func(td store.TrainDoc) {
		m.add(Tokenize(td.Content), td.CorrespondentID, td.DocumentTypeID, td.TagIDs)
	})
	if err != nil {
		return err
	}
	m.finalize()
	c.model.Store(m)
	c.log.Info("classifier trained", "documents", m.n, "vocabulary", len(m.vocab), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// Classify combines explicit rules with the learned model. Rules win for
// correspondent and type; tags from both are merged.
func (c *Classifier) Classify(ctx context.Context, content string) (Prediction, error) {
	return c.ClassifyWith(ctx, content, Hint{})
}

// ClassifyWith is Classify with the sender possibly decided already.
func (c *Classifier) ClassifyWith(ctx context.Context, content string, h Hint) (Prediction, error) {
	var p Prediction
	corrs, err := c.store.ListEntities(ctx, store.KindCorrespondent)
	if err != nil {
		return p, err
	}
	types, err := c.store.ListEntities(ctx, store.KindDocumentType)
	if err != nil {
		return p, err
	}
	tags, err := c.store.ListEntities(ctx, store.KindTag)
	if err != nil {
		return p, err
	}
	m := c.model.Load()
	var toks []string
	var nbs []neighbor
	if m != nil && m.n > 0 {
		toks = Tokenize(content)
		nbs = m.neighbors(toks, nil)
	}

	if h.SenderSet {
		p.CorrespondentID = h.CorrespondentID
		if p.CorrespondentID != nil {
			p.CorrespondentFrom = FromGiven
		}
		if toks != nil {
			if cid := h.CorrespondentID; cid != nil {
				// Only this sender's documents say anything about type and tags.
				nbs = m.neighbors(toks, func(l label) bool { return l.corr == *cid })
			} else {
				nbs = nil // a new sender: nothing learned applies yet
			}
		}
	} else {
		p.CorrespondentID, p.CorrespondentFrom = pickOne(corrs, content, m, nbs, func(l label) int64 { return l.corr })
		if p.CorrespondentID != nil && toks != nil {
			// Same sender, similar document: that's the best guide for type and tags.
			cid := *p.CorrespondentID
			if same := m.neighbors(toks, func(l label) bool { return l.corr == cid }); len(same) > 0 {
				nbs = same
			}
		}
	}
	p.DocumentTypeID, p.DocumentTypeFrom = pickOne(types, content, m, nbs, func(l label) int64 { return l.typ })

	auto := map[int64]bool{}
	seen := map[int64]bool{}
	for _, t := range tags {
		if t.Algorithm == "auto" {
			auto[t.ID] = true
		} else if store.Matches(t, content) {
			seen[t.ID] = true
			p.TagIDs = append(p.TagIDs, t.ID)
		}
	}
	if len(nbs) > 0 {
		for _, id := range m.predictTags(nbs) {
			if auto[id] && !seen[id] {
				seen[id] = true
				p.TagIDs = append(p.TagIDs, id)
			}
		}
	}
	sort.Slice(p.TagIDs, func(i, j int) bool { return p.TagIDs[i] < p.TagIDs[j] })
	return p, nil
}

func pickOne(ents []store.Entity, content string, m *model, nbs []neighbor, of func(label) int64) (*int64, string) {
	auto := map[int64]bool{}
	for _, e := range ents {
		if e.Algorithm == "auto" {
			auto[e.ID] = true
			continue
		}
		if store.Matches(e, content) {
			id := e.ID
			return &id, FromRule
		}
	}
	if len(nbs) == 0 {
		return nil, ""
	}
	if id, ok, strong := m.predictOne(nbs, of); ok && auto[id] {
		if strong {
			return &id, FromLearned
		}
		return &id, FromWeak
	}
	return nil, ""
}

// Tokenize returns the distinct lower-cased words of a text, skipping numbers
// and very short or very long tokens.
func Tokenize(s string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		n := len([]rune(f))
		if n < 3 || n > 30 {
			continue
		}
		hasLetter := false
		for _, r := range f {
			if unicode.IsLetter(r) {
				hasLetter = true
				break
			}
		}
		if !hasLetter || isStopword(f) {
			continue
		}
		if _, ok := seen[f]; !ok {
			seen[f] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

// ---- nearest neighbours ----
//
// Every reviewed document is a TF-IDF vector over its distinct words. A new
// document gets the labels shared by its most similar reviewed documents.
// This works from the very first example, which suits personal archives
// where most documents come from senders seen before.

type posting struct {
	doc int32
	w   float32
}

type label struct {
	corr, typ int64
	tags      []int64
}

type model struct {
	n        int
	vocab    map[string]int32
	idf      []float32
	postings [][]posting
	labels   []label
	pending  [][]int32 // token ids per document, only during training
}

func newModel() *model { return &model{vocab: map[string]int32{}} }

func (m *model) add(toks []string, corr, typ int64, tags []int64) {
	ids := make([]int32, 0, len(toks))
	for _, w := range toks {
		id, ok := m.vocab[w]
		if !ok {
			id = int32(len(m.vocab))
			m.vocab[w] = id
		}
		ids = append(ids, id)
	}
	m.pending = append(m.pending, ids)
	m.labels = append(m.labels, label{corr, typ, tags})
	m.n++
}

func (m *model) finalize() {
	df := make([]int, len(m.vocab))
	for _, ids := range m.pending {
		for _, id := range ids {
			df[id]++
		}
	}
	m.idf = make([]float32, len(m.vocab))
	for i, d := range df {
		// A word in every document (like your own name and address) gets
		// weight 0, so it can't make unrelated letters look alike.
		m.idf[i] = float32(math.Log(float64(m.n+1) / float64(d+1)))
	}
	m.postings = make([][]posting, len(m.vocab))
	for d, ids := range m.pending {
		var norm float64
		for _, id := range ids {
			norm += float64(m.idf[id]) * float64(m.idf[id])
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			continue
		}
		for _, id := range ids {
			m.postings[id] = append(m.postings[id], posting{int32(d), float32(float64(m.idf[id]) / norm)})
		}
	}
	m.pending = nil
}

type neighbor struct {
	doc int
	sim float64
}

const (
	maxNeighbors  = 7
	minSimilarity = 0.15
)

// neighbors returns the most similar reviewed documents (optionally only
// those passing keep): up to maxNeighbors, and only those at least half as
// similar as the best match.
func (m *model) neighbors(toks []string, keep func(label) bool) []neighbor {
	if m.n == 0 {
		return nil
	}
	type qt struct {
		id int32
		w  float64
	}
	var q []qt
	var norm float64
	for _, w := range toks {
		if id, ok := m.vocab[w]; ok {
			x := float64(m.idf[id])
			q = append(q, qt{id, x})
			norm += x * x
		}
	}
	if norm == 0 {
		return nil
	}
	norm = math.Sqrt(norm)
	scores := make([]float64, m.n)
	for _, t := range q {
		for _, p := range m.postings[t.id] {
			scores[p.doc] += t.w / norm * float64(p.w)
		}
	}
	var out []neighbor
	for d, s := range scores {
		if s >= minSimilarity && (keep == nil || keep(m.labels[d])) {
			out = append(out, neighbor{d, s})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sim > out[j].sim })
	if len(out) > maxNeighbors {
		out = out[:maxNeighbors]
	}
	for i, nb := range out {
		if nb.sim < out[0].sim/2 {
			return out[:i]
		}
	}
	return out
}

// vote picks the label that holds the majority of neighbour similarity.
// strongSimilarity is how alike the best match must be (with a second
// agreeing document) to count as strong evidence.
const strongSimilarity = 0.35

// vote picks the label holding the majority of neighbour similarity. strong
// means at least two documents agree and the best one is clearly similar.
func vote(nbs []neighbor, of func(int) int64) (best int64, ok, strong bool) {
	weights := map[int64]float64{}
	count := map[int64]int{}
	var total float64
	for _, nb := range nbs {
		weights[of(nb.doc)] += nb.sim
		count[of(nb.doc)]++
		total += nb.sim
	}
	bestW := 0.0
	for id, w := range weights {
		if w > bestW {
			best, bestW = id, w
		}
	}
	ok = best != 0 && bestW > total/2
	strong = ok && count[best] >= 2 && len(nbs) > 0 && nbs[0].sim >= strongSimilarity
	return best, ok, strong
}

func (m *model) predictOne(nbs []neighbor, of func(label) int64) (int64, bool, bool) {
	return vote(nbs, func(d int) int64 { return of(m.labels[d]) })
}

func (m *model) predictTags(nbs []neighbor) []int64 {
	weights := map[int64]float64{}
	var total float64
	for _, nb := range nbs {
		total += nb.sim
		for _, t := range m.labels[nb.doc].tags {
			weights[t] += nb.sim
		}
	}
	var out []int64
	for t, w := range weights {
		if w > total/2 {
			out = append(out, t)
		}
	}
	return out
}
