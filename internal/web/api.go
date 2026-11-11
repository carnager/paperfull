package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"paperfull/internal/ingest"
	"paperfull/internal/ocr"
	"paperfull/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "request canceled")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", r.PathValue("id"))
	}
	return id, nil
}

func pathKind(r *http.Request) (store.Kind, error) {
	k := store.Kind(r.PathValue("kind"))
	switch k {
	case "types":
		k = store.KindDocumentType
	case "senders":
		k = store.KindCorrespondent
	}
	if !k.Valid() {
		return "", store.ErrNotFound
	}
	return k, nil
}

// ---- documents ----

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	res, err := s.Store.Search(r.Context(), store.SearchOptions{
		Query: q.Get("q"), Sort: q.Get("sort"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiGetDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.Store.GetDocument(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// resolveRef turns an id or a name into an entity id; unknown names are created.
func (s *Server) resolveRef(ctx context.Context, k store.Kind, v string) (int64, error) {
	v = strings.TrimSpace(v)
	if id, err := strconv.ParseInt(v, 10, 64); err == nil {
		if _, err := s.Store.GetEntity(ctx, k, id); err != nil {
			return 0, fmt.Errorf("%s %d: %w", k.Singular(), id, err)
		}
		return id, nil
	}
	return s.Store.EnsureEntity(ctx, k, v)
}

func (s *Server) resolveJSONRef(ctx context.Context, k store.Kind, raw json.RawMessage) (*store.NullID, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case nil:
		return &store.NullID{}, nil
	case float64:
		id, err := s.resolveRef(ctx, k, strconv.FormatInt(int64(x), 10))
		return &store.NullID{ID: id, Valid: true}, err
	case string:
		if x == "" {
			return &store.NullID{}, nil
		}
		id, err := s.resolveRef(ctx, k, x)
		return &store.NullID{ID: id, Valid: true}, err
	}
	return nil, fmt.Errorf("%s: expected id, name or null", k.Singular())
}

func (s *Server) resolveJSONTags(ctx context.Context, raw json.RawMessage) ([]int64, error) {
	var vs []any
	if err := json.Unmarshal(raw, &vs); err != nil {
		return nil, errors.New("tags: expected a list of ids or names")
	}
	ids := []int64{}
	for _, v := range vs {
		var s2 string
		switch x := v.(type) {
		case float64:
			s2 = strconv.FormatInt(int64(x), 10)
		case string:
			s2 = x
		default:
			return nil, errors.New("tags: expected ids or names")
		}
		id, err := s.resolveRef(ctx, store.KindTag, s2)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// parseUpdate reads the fields of a PATCH/bulk body into a DocUpdate.
func (s *Server) parseUpdate(ctx context.Context, body map[string]json.RawMessage) (store.DocUpdate, error) {
	var u store.DocUpdate
	str := func(key string) (*string, error) {
		raw, ok := body[key]
		if !ok {
			return nil, nil
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s: expected a string", key)
		}
		return &v, nil
	}
	var err error
	if u.Title, err = str("title"); err != nil {
		return u, err
	}
	if u.Created, err = str("created"); err != nil {
		return u, err
	}
	if u.Notes, err = str("notes"); err != nil {
		return u, err
	}
	if raw, ok := body["inbox"]; ok {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return u, errors.New("inbox: expected true or false")
		}
		u.Inbox = &b
	}
	if raw, ok := body["correspondent"]; ok {
		if u.Correspondent, err = s.resolveJSONRef(ctx, store.KindCorrespondent, raw); err != nil {
			return u, err
		}
	}
	if raw, ok := body["document_type"]; ok {
		if u.DocumentType, err = s.resolveJSONRef(ctx, store.KindDocumentType, raw); err != nil {
			return u, err
		}
	}
	if raw, ok := body["tags"]; ok {
		ids, err := s.resolveJSONTags(ctx, raw)
		if err != nil {
			return u, err
		}
		u.Tags = &ids
	}
	if raw, ok := body["add_tags"]; ok {
		if u.AddTags, err = s.resolveJSONTags(ctx, raw); err != nil {
			return u, err
		}
	}
	if raw, ok := body["remove_tags"]; ok {
		if u.RemoveTags, err = s.resolveJSONTags(ctx, raw); err != nil {
			return u, err
		}
	}
	return u, nil
}

// applyUpdate saves changes, renames files to match and schedules retraining.
func (s *Server) applyUpdate(ctx context.Context, ids []int64, u store.DocUpdate) error {
	if u.Empty() {
		return nil
	}
	if err := s.Store.UpdateDocuments(ctx, ids, u); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.Ingester.Relocate(ctx, id); err != nil {
			s.Log.Warn("renaming files", "id", id, "err", err)
		}
	}
	s.touched()
	return nil
}

func (s *Server) apiPatchDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	u, err := s.parseUpdate(r.Context(), body)
	if err == nil {
		err = s.applyUpdate(r.Context(), []int64{id}, u)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.apiGetDocument(w, r)
}

func (s *Server) apiBulk(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var ids []int64
	if err := json.Unmarshal(body["documents"], &ids); err != nil || len(ids) == 0 {
		writeError(w, http.StatusBadRequest, `"documents" must be a non-empty list of ids`)
		return
	}
	delete(body, "documents")
	if raw, ok := body["delete"]; ok {
		var del bool
		json.Unmarshal(raw, &del)
		if del {
			for _, id := range ids {
				if err := s.Ingester.Delete(r.Context(), id); err != nil {
					writeErr(w, err)
					return
				}
			}
			s.touched()
			writeJSON(w, http.StatusOK, map[string]any{"deleted": ids})
			return
		}
	}
	u, err := s.parseUpdate(r.Context(), body)
	if err == nil {
		err = s.applyUpdate(r.Context(), ids, u)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": ids})
}

func (s *Server) apiDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Ingester.Delete(r.Context(), id)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.touched()
	w.WriteHeader(http.StatusNoContent)
}

// apiUpload accepts either multipart/form-data (field "document" or "file")
// or the raw file as body with ?filename=. Metadata can be preset with
// title, created, correspondent, document_type and tags (ids or names).
// With ?wait=true the response is sent when processing is finished.
func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tmp, err := os.CreateTemp(s.Ingester.TmpDir(), "upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmpPath := tmp.Name()
	cleanup := func() { os.Remove(tmpPath) }

	field := func(k string) string { return r.FormValue(k) }
	var multi func(k string) []string
	name := ""
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			tmp.Close()
			cleanup()
			writeError(w, http.StatusBadRequest, "reading upload: "+err.Error())
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, hdr, err := r.FormFile("document")
		if err != nil {
			f, hdr, err = r.FormFile("file")
		}
		if err != nil {
			tmp.Close()
			cleanup()
			writeError(w, http.StatusBadRequest, `missing file field "document"`)
			return
		}
		name = hdr.Filename
		_, err = io.Copy(tmp, f)
		f.Close()
		if err != nil {
			tmp.Close()
			cleanup()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		multi = func(k string) []string { return r.MultipartForm.Value[k] }
	} else {
		if _, err := io.Copy(tmp, r.Body); err != nil {
			tmp.Close()
			cleanup()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		name = r.URL.Query().Get("filename")
		if name == "" {
			name = r.Header.Get("X-Filename")
		}
		multi = func(k string) []string { return r.URL.Query()[k] }
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		name = "upload"
	}

	opts, err := s.uploadOptions(ctx, field, multi)
	opts.Source = "upload"
	if err != nil {
		cleanup()
		writeErr(w, err)
		return
	}
	task := s.Ingester.Submit(tmpPath, name, opts, func(ingest.Task) { cleanup() })
	if v, _ := strconv.ParseBool(r.URL.Query().Get("wait")); v {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		task, _ = s.Ingester.Wait(wctx, task.ID)
		status := http.StatusOK
		switch task.Status {
		case ingest.StatusFailed:
			status = http.StatusUnprocessableEntity
		case ingest.StatusDuplicate:
			status = http.StatusConflict
		case ingest.StatusQueued, ingest.StatusProcessing:
			status = http.StatusAccepted
		}
		writeJSON(w, status, task)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) uploadOptions(ctx context.Context, field func(string) string, multi func(string) []string) (ingest.Options, error) {
	var o ingest.Options
	o.Title = field("title")
	if c := field("created"); c != "" {
		if !store.ValidDate(c) {
			return o, fmt.Errorf("created must be YYYY-MM-DD")
		}
		o.Created = c
	}
	if v := field("correspondent"); v != "" {
		id, err := s.resolveRef(ctx, store.KindCorrespondent, v)
		if err != nil {
			return o, err
		}
		o.CorrespondentID = &id
	}
	if v := field("document_type"); v != "" {
		id, err := s.resolveRef(ctx, store.KindDocumentType, v)
		if err != nil {
			return o, err
		}
		o.DocumentTypeID = &id
	}
	for _, v := range multi("tags") {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			id, err := s.resolveRef(ctx, store.KindTag, part)
			if err != nil {
				return o, err
			}
			o.TagIDs = append(o.TagIDs, id)
		}
	}
	return o, nil
}

func (s *Server) serveFile(preferArchive, download bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		d, err := s.Store.GetDocument(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		path, ctype, name := filepath.Join(s.Ingester.OriginalsDir(), d.Path), d.Mime, d.OriginalName
		if preferArchive && d.ArchivePath != "" {
			path, ctype = filepath.Join(s.Ingester.ArchiveDir(), d.ArchivePath), "application/pdf"
			name = strings.TrimSuffix(name, filepath.Ext(name)) + ".pdf"
		}
		f, err := os.Open(path)
		if err != nil {
			writeError(w, http.StatusNotFound, "file missing on disk")
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		disp := "inline"
		if download {
			disp = "attachment"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
		http.ServeContent(w, r, "", fi.ModTime(), f)
	}
}

func (s *Server) serveThumb(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	p := s.Ingester.ThumbPath(id)
	if _, err := os.Stat(p); err != nil {
		// No rendered page (office or text file): draw a page with the file type on it.
		label := "FILE"
		if d, err := s.Store.GetDocument(r.Context(), id); err == nil {
			label = ocr.Label(d.Mime)
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-store") // a real thumbnail may follow soon
		fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 160 128"><rect width="160" height="128" fill="#8881"/>`+
			`<path d="M58 14h32l16 16v60a4 4 0 0 1-4 4H58a4 4 0 0 1-4-4V18a4 4 0 0 1 4-4z" fill="#fff" stroke="#8886" stroke-width="2"/>`+
			`<path d="M90 14v16h16" fill="none" stroke="#8886" stroke-width="2"/>`+
			`<path d="M64 46h32M64 56h32M64 66h22" stroke="#8885" stroke-width="3" stroke-linecap="round"/>`+
			`<text x="80" y="114" text-anchor="middle" font-family="system-ui,sans-serif" font-size="13" font-weight="700" fill="#888">%s</text></svg>`, label)
		return
	}
	// The page links thumbnails with ?v=<last change>, so a versioned URL
	// never changes its content and can be cached for good.
	if r.URL.Query().Has("v") {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	http.ServeFile(w, r, p)
}

func (s *Server) apiSuggestions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.Store.GetDocument(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := s.Cls.Classify(r.Context(), d.Content)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.TagIDs == nil {
		p.TagIDs = []int64{}
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiReprocess(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		_, err = s.Store.GetDocument(r.Context(), id)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Ingester.Reprocess(id))
}

func (s *Server) apiTasks(w http.ResponseWriter, r *http.Request) {
	tasks := s.Ingester.Tasks(200)
	if tasks == nil {
		tasks = []ingest.Task{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) apiTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Ingester.Task(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such task")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) apiRetrain(w http.ResponseWriter, r *http.Request) {
	if err := s.Cls.Train(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "trained"})
}

// ---- tags, correspondents, document types ----

func (s *Server) apiListEntities(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	es, err := s.Store.ListEntities(r.Context(), k)
	if err != nil {
		writeErr(w, err)
		return
	}
	if es == nil {
		es = []store.Entity{}
	}
	writeJSON(w, http.StatusOK, es)
}

func (s *Server) apiGetEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	e, err := s.Store.GetEntity(r.Context(), k, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) apiCreateEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var e store.Entity
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	id, err := s.Store.CreateEntity(r.Context(), k, e)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.touched()
	out, _ := s.Store.GetEntity(r.Context(), k, id)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) apiUpdateEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	cur, err := s.Store.GetEntity(r.Context(), k, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Decode over the current values so omitted fields stay unchanged.
	if err := json.NewDecoder(r.Body).Decode(cur); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	cur.ID = id
	if err := s.Store.UpdateEntity(r.Context(), k, *cur); err != nil {
		writeErr(w, err)
		return
	}
	s.relocateEntityDocs(r.Context(), k, id)
	s.touched()
	out, _ := s.Store.GetEntity(r.Context(), k, id)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiDeleteEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	ids, err := s.Store.DeleteEntity(r.Context(), k, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.relocate(r.Context(), ids)
	s.touched()
	w.WriteHeader(http.StatusNoContent)
}

// relocateEntityDocs renames files of documents whose path may include the
// entity's name.
func (s *Server) relocateEntityDocs(ctx context.Context, k store.Kind, id int64) {
	e, err := s.Store.GetEntity(ctx, k, id)
	if err != nil {
		return
	}
	var query string
	switch k {
	case store.KindTag:
		query = "tag:" + quoteQ(e.Name)
	case store.KindCorrespondent:
		query = "correspondent:" + quoteQ(e.Name)
	default:
		query = "type:" + quoteQ(e.Name)
	}
	for off := 0; ; off += 500 {
		res, err := s.Store.Search(ctx, store.SearchOptions{Query: query, Limit: 500, Offset: off})
		if err != nil || len(res.Documents) == 0 {
			return
		}
		ids := make([]int64, len(res.Documents))
		for i, d := range res.Documents {
			ids[i] = d.ID
		}
		s.relocate(ctx, ids)
	}
}

func (s *Server) relocate(ctx context.Context, ids []int64) {
	for _, id := range ids {
		if err := s.Ingester.Relocate(ctx, id); err != nil {
			s.Log.Warn("renaming files", "id", id, "err", err)
		}
	}
}

func quoteQ(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, "") + `"`
}

// apiAISuggest asks the language model about a stored document. Nothing is
// saved; the page fills its form fields with the answer.
func (s *Server) apiAISuggest(w http.ResponseWriter, r *http.Request) {
	if !s.Ingester.AIEnabled() {
		writeError(w, http.StatusNotFound, "AI suggestions are off (start paperfull with -ai)")
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.Store.GetDocument(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	sug, sid, tid, err := s.Ingester.AISuggest(r.Context(), d.Content)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sender": sug.Sender, "type": sug.Type, "title": sug.Title, "date": sug.Date,
		"sender_exists": sid != nil, "type_exists": tid != nil,
	})
}
