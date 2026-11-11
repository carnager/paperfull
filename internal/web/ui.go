package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"paperfull/internal/ocr"
	"paperfull/internal/store"
)

// ---- template helpers ----

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Swatches offered for tag colors.
var swatches = []string{"#e5484d", "#f76b15", "#ffc53d", "#46a758", "#12a594", "#0090ff", "#3e63dd", "#8e4ec6", "#d6409f", "#8d8d86"}

var funcs = template.FuncMap{
	"niceDate": niceDate,
	"ago":      ago,
	"inc":      func(i int) int { return i + 1 },
	"dec":      func(i int) int { return i - 1 },
	"safe":     func(s string) template.HTML { return template.HTML(s) },
	"withF": func(q, key, value string) string {
		return store.WithFilter(q, key, value, false)
	},
	"listURL":   listURL,
	"kindLabel": kindLabel,
	"kindOne":   kindOne,
	"algos":     func() []string { return store.Algorithms },
	"algoLabel": algoLabel,
	"algoSummary": func(e store.Entity) string {
		m := e.Match
		switch e.Algorithm {
		case "auto":
			return "Learns from your documents"
		case "none":
			return "Only assigned by hand"
		case "any":
			return "Text contains any of: " + m
		case "all":
			return "Text contains all of: " + m
		case "literal":
			return "Text contains “" + m + "”"
		case "regex":
			return "Text matches /" + m + "/"
		}
		return ""
	},
	"swatches": func() []string { return swatches },
	"tagStyle": func(color string) template.CSS {
		if hexColor.MatchString(color) {
			return template.CSS("--c:" + color)
		}
		return ""
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"statusText": func(s string) string {
		switch s {
		case "queued":
			return "Waiting"
		case "processing":
			return "Reading text…"
		case "done":
			return "Added"
		case "duplicate":
			return "Already in your archive"
		case "failed":
			return "Couldn't be added"
		}
		return s
	},
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	},
}

func niceDate(s string) string {
	for _, layout := range []struct{ in, out string }{
		{"2006-01-02", "2 Jan 2006"}, {"2006-01", "Jan 2006"}, {"2006", "2006"},
	} {
		if t, err := time.Parse(layout.in, s); err == nil {
			return t.Format(layout.out)
		}
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return t.Local().Format("2 Jan 2006")
}

func listURL(path, q, sort string, page int) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

func kindLabel(k store.Kind) string {
	switch k {
	case store.KindTag:
		return "Tags"
	case store.KindCorrespondent:
		return "Senders"
	}
	return "Types"
}

func kindOne(k store.Kind) string {
	switch k {
	case store.KindTag:
		return "tag"
	case store.KindCorrespondent:
		return "sender"
	}
	return "type"
}

func algoLabel(a string) string {
	switch a {
	case "auto":
		return "Learn from my documents"
	case "any":
		return "When the text contains any of these words"
	case "all":
		return "When the text contains all of these words"
	case "literal":
		return "When the text contains this exact phrase"
	case "regex":
		return "When the text matches a regular expression"
	case "none":
		return "Never, I assign it myself"
	}
	return a
}

// ---- page plumbing ----

type Base struct {
	Title      string
	Active     string
	Auth       bool
	InboxCount int
	Total      int
}

func (s *Server) base(ctx context.Context, title, active string) Base {
	st, _ := s.Store.Stats(ctx)
	return Base{Title: title, Active: active, Auth: s.Token != "", InboxCount: st.Inbox, Total: st.Documents}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.Log.Error("template", "name", name, "err", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) uiError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	msg := err.Error()
	if errors.Is(err, store.ErrNotFound) {
		status, msg = http.StatusNotFound, "This page or document doesn't exist (anymore)."
	}
	w.WriteHeader(status)
	s.render(w, "error", struct {
		Base
		Err string
	}{s.base(r.Context(), "Something went wrong", ""), msg})
}

// redirectMsg redirects and lets the next page show a short notice.
func redirectMsg(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

type Entities struct {
	Tags, Corrs, Types []store.Entity
}

func (s *Server) loadEntities(ctx context.Context) (Entities, error) {
	var e Entities
	var err error
	if e.Tags, err = s.Store.ListEntities(ctx, store.KindTag); err != nil {
		return e, err
	}
	if e.Corrs, err = s.Store.ListEntities(ctx, store.KindCorrespondent); err != nil {
		return e, err
	}
	e.Types, err = s.Store.ListEntities(ctx, store.KindDocumentType)
	return e, err
}

// ---- document list ----

type chip struct {
	Label, Value string
	Remove       string // query without this filter
}

func chipsFor(toks []store.QueryToken) (text string, chips []chip, raws []string) {
	var texts []string
	for i, t := range toks {
		if t.Key == "" {
			texts = append(texts, t.Raw)
			continue
		}
		raws = append(raws, t.Raw)
		rest := append(append([]store.QueryToken{}, toks[:i]...), toks[i+1:]...)
		c := chip{Value: t.Value, Remove: store.JoinQuery(rest)}
		none := strings.EqualFold(t.Value, "none")
		switch t.Key {
		case "tag":
			c.Label = map[bool]string{false: "Tag", true: "Without tag"}[t.Neg]
			if none {
				c.Label, c.Value = map[bool]string{false: "No tags", true: "Has tags"}[t.Neg], ""
			}
		case "from":
			c.Label = map[bool]string{false: "From", true: "Not from"}[t.Neg]
			if none {
				c.Label, c.Value = "No sender", ""
			}
		case "type":
			c.Label = map[bool]string{false: "Type", true: "Not type"}[t.Neg]
			if none {
				c.Label, c.Value = "No type", ""
			}
		case "after":
			c.Label, c.Value = "From", niceDate(t.Value)
		case "before":
			c.Label, c.Value = "Before", niceDate(t.Value)
		case "year", "created":
			c.Label, c.Value = "Date", niceDate(t.Value)
		case "inbox":
			yes := strings.EqualFold(t.Value, "yes") || t.Value == "true" || t.Value == "1"
			c.Label, c.Value = map[bool]string{true: "In inbox", false: "Reviewed"}[yes != t.Neg], ""
		case "title":
			c.Label = "Title contains"
		case "id":
			c.Label = "Document"
		}
		chips = append(chips, c)
	}
	return strings.Join(texts, " "), chips, raws
}

const pageSize = 48

func (s *Server) uiList(w http.ResponseWriter, r *http.Request)  { s.list(w, r, false) }
func (s *Server) uiInbox(w http.ResponseWriter, r *http.Request) { s.list(w, r, true) }

func (s *Server) list(w http.ResponseWriter, r *http.Request, inbox bool) {
	ctx := r.Context()
	v := r.URL.Query()
	q := v.Get("q")
	// The search form sends free text and the active filters separately.
	if v.Has("text") || v.Has("f") {
		q = strings.TrimSpace(strings.Join(append(v["f"], v.Get("text")), " "))
	}
	if from := v.Get("from"); store.ValidDate(from) {
		q = store.WithFilter(q, "after", from, false)
	}
	if to := v.Get("to"); store.ValidDate(to) {
		t, _ := time.Parse("2006-01-02", to)
		q = store.WithFilter(q, "before", t.AddDate(0, 0, 1).Format("2006-01-02"), false)
	}
	sort := v.Get("sort")
	page, _ := strconv.Atoi(v.Get("page"))
	page = max(page, 1)

	path, title, active := "/", "All documents", "all"
	effective := q
	if inbox {
		path, title, active = "/inbox", "Inbox", "inbox"
		effective = "inbox:yes " + q
	}
	text, chips, raws := chipsFor(store.ParseQuery(q))
	data := struct {
		Base
		Entities
		Path, Q, Text, Sort, Err, Msg, ConsumeDir string
		Chips                                     []chip
		Raws                                      []string
		Inbox                                     bool
		Res                                       *store.SearchResult
		Page, Pages                               int
		Years                                     []store.YearCount
		FirstInbox                                int64
	}{Base: s.base(ctx, title, active), Path: path, Q: q, Text: text, Sort: sort, Chips: chips, Raws: raws,
		Inbox: inbox, Page: page, Msg: v.Get("msg"), ConsumeDir: s.ConsumeDir}

	var err error
	if data.Entities, err = s.loadEntities(ctx); err != nil {
		s.uiError(w, r, err)
		return
	}
	data.Years, _ = s.Store.Years(ctx)
	res, err := s.Store.Search(ctx, store.SearchOptions{Query: effective, Sort: sort, Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		data.Err = strings.TrimPrefix(err.Error(), "could not understand search query: ")
		res = &store.SearchResult{Documents: []*store.Document{}}
	}
	data.Res = res
	data.Pages = max(1, (res.Total+pageSize-1)/pageSize)
	if inbox {
		data.FirstInbox, _ = s.Store.NextInbox(ctx, 0)
	}
	if v.Get("partial") == "1" {
		s.render(w, "listview", data)
		return
	}
	s.render(w, "list", data)
}

func formIDs(r *http.Request, key string) []int64 {
	var ids []int64
	for _, v := range r.Form[key] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func safeReturn(v string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return v
	}
	return "/"
}

// refByName turns a typed name into a reference: "" clears, unknown names are created.
func (s *Server) refByName(ctx context.Context, k store.Kind, name string) (*store.NullID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return &store.NullID{}, nil
	}
	id, err := s.Store.EnsureEntity(ctx, k, name)
	if err != nil {
		return nil, err
	}
	return &store.NullID{ID: id, Valid: true}, nil
}

func (s *Server) tagsByName(ctx context.Context, list string) ([]int64, error) {
	ids := []int64{}
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		id, err := s.Store.EnsureEntity(ctx, store.KindTag, name)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Server) uiBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.uiError(w, r, err)
		return
	}
	ids := formIDs(r, "ids")
	ret := safeReturn(r.FormValue("return"))
	if len(ids) == 0 {
		http.Redirect(w, r, ret, http.StatusSeeOther)
		return
	}
	n := len(ids)
	docs := map[bool]string{true: "document", false: "documents"}[n == 1]
	var u store.DocUpdate
	var msg string
	var err error
	switch r.FormValue("op") {
	case "delete":
		for _, id := range ids {
			if err := s.Ingester.Delete(ctx, id); err != nil {
				s.uiError(w, r, err)
				return
			}
		}
		s.touched()
		redirectMsg(w, r, ret, fmt.Sprintf("Deleted %d %s", n, docs))
		return
	case "reprocess":
		for _, id := range ids {
			s.Ingester.Reprocess(id)
		}
		redirectMsg(w, r, "/activity", fmt.Sprintf("Reading %d %s again", n, docs))
		return
	case "reviewed":
		f := false
		u.Inbox, msg = &f, fmt.Sprintf("Marked %d %s as reviewed", n, docs)
	case "inbox":
		t := true
		u.Inbox, msg = &t, fmt.Sprintf("Moved %d %s to the inbox", n, docs)
	case "tag_add", "tag_remove":
		name := strings.TrimSpace(r.FormValue("tag"))
		if name == "" {
			break
		}
		if r.FormValue("op") == "tag_add" {
			u.AddTags, err = s.tagsByName(ctx, name)
			msg = fmt.Sprintf("Tagged %d %s “%s”", n, docs, name)
		} else if e, e2 := s.Store.EntityByName(ctx, store.KindTag, name); e2 == nil {
			u.RemoveTags = []int64{e.ID}
			msg = fmt.Sprintf("Removed “%s” from %d %s", name, n, docs)
		}
	case "sender":
		u.Correspondent, err = s.refByName(ctx, store.KindCorrespondent, r.FormValue("sender"))
		msg = fmt.Sprintf("Updated sender of %d %s", n, docs)
	case "type":
		u.DocumentType, err = s.refByName(ctx, store.KindDocumentType, r.FormValue("type"))
		msg = fmt.Sprintf("Updated type of %d %s", n, docs)
	}
	if err == nil {
		err = s.applyUpdate(ctx, ids, u)
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	redirectMsg(w, r, ret, msg)
}

// ---- single document ----

func (s *Server) uiDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	d, err := s.Store.GetDocument(ctx, id)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	type tagInfo struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	data := struct {
		Base
		Entities
		Doc       *store.Document
		TagNames  string
		AllTags   []tagInfo
		NextInbox int64
		AI        bool
		Back      string
		Preview   string
		Msg       string
	}{Base: s.base(ctx, d.Title, "all"), Doc: d, Msg: r.URL.Query().Get("msg"), Back: "/"}
	if d.Inbox {
		data.Active, data.Back = "inbox", "/inbox"
	}
	if data.Entities, err = s.loadEntities(ctx); err != nil {
		s.uiError(w, r, err)
		return
	}
	names := make([]string, len(d.Tags))
	for i, t := range d.Tags {
		names[i] = t.Name
	}
	data.TagNames = strings.Join(names, ", ")
	data.AllTags = []tagInfo{}
	for _, t := range data.Tags {
		data.AllTags = append(data.AllTags, tagInfo{t.Name, t.Color})
	}
	switch {
	case d.ArchivePath != "" || d.Mime == "application/pdf":
		data.Preview = "pdf"
	case strings.HasPrefix(d.Mime, "image/"):
		data.Preview = "image"
	case ocr.IsOffice(d.Mime):
		data.Preview = "office"
	default:
		data.Preview = "text"
	}
	data.NextInbox, _ = s.Store.NextInbox(ctx, id)
	data.AI = s.Ingester.AIEnabled()
	s.render(w, "document", data)
}

func (s *Server) uiSaveDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.uiError(w, r, err)
		return
	}
	title, created, notes := r.FormValue("title"), r.FormValue("created"), r.FormValue("notes")
	u := store.DocUpdate{Title: &title, Created: &created, Notes: &notes}
	tags, err := s.tagsByName(ctx, r.FormValue("tags"))
	if err == nil {
		u.Tags = &tags
		u.Correspondent, err = s.refByName(ctx, store.KindCorrespondent, r.FormValue("sender"))
	}
	if err == nil {
		u.DocumentType, err = s.refByName(ctx, store.KindDocumentType, r.FormValue("type"))
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	op := r.FormValue("op")
	switch op {
	case "done":
		f := false
		u.Inbox = &f
	case "to_inbox":
		t := true
		u.Inbox = &t
	}
	if err := s.applyUpdate(ctx, []int64{id}, u); err != nil {
		s.uiError(w, r, err)
		return
	}
	switch op {
	case "done":
		if next, ok := s.Store.NextInbox(ctx, id); ok {
			redirectMsg(w, r, fmt.Sprintf("/documents/%d", next), "Saved. Here's the next one.")
			return
		}
		redirectMsg(w, r, "/inbox", "All done, your inbox is empty.")
	case "to_inbox":
		redirectMsg(w, r, fmt.Sprintf("/documents/%d", id), "Moved back to the inbox")
	default:
		redirectMsg(w, r, fmt.Sprintf("/documents/%d", id), "Saved")
	}
}

func (s *Server) uiDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Ingester.Delete(r.Context(), id)
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.touched()
	redirectMsg(w, r, safeReturn(r.FormValue("return")), "Document deleted")
}

func (s *Server) uiReprocess(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.Ingester.Reprocess(id)
	redirectMsg(w, r, "/activity", "Reading the document again")
}

// ---- tags, senders, types ----

func (s *Server) uiOrganize(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	es, err := s.Store.ListEntities(r.Context(), k)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	filterKey := map[store.Kind]string{store.KindTag: "tag", store.KindCorrespondent: "from", store.KindDocumentType: "type"}[k]
	s.render(w, "organize", struct {
		Base
		Kind      store.Kind
		Entities  []store.Entity
		FilterKey string
		Err, Msg  string
	}{s.base(r.Context(), kindLabel(k), string(k)), k, es, filterKey, r.URL.Query().Get("err"), r.URL.Query().Get("msg")})
}

func entityFromForm(r *http.Request) store.Entity {
	return store.Entity{
		Name:          r.FormValue("name"),
		Match:         r.FormValue("match"),
		Algorithm:     r.FormValue("algorithm"),
		CaseSensitive: r.FormValue("case_sensitive") == "1",
		Color:         r.FormValue("color"),
	}
}

func organizeRedirect(w http.ResponseWriter, r *http.Request, k store.Kind, msg string, err error) {
	u := "/organize/" + string(k)
	if err != nil {
		http.Redirect(w, r, u+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	redirectMsg(w, r, u, msg)
}

func (s *Server) uiCreateEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	r.ParseForm()
	e := entityFromForm(r)
	if _, err = s.Store.CreateEntity(r.Context(), k, e); err == nil {
		s.touched()
	}
	organizeRedirect(w, r, k, fmt.Sprintf("Created %s “%s”", kindOne(k), strings.TrimSpace(e.Name)), err)
}

func (s *Server) uiUpdateEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	r.ParseForm()
	e := entityFromForm(r)
	e.ID = id
	err = s.Store.UpdateEntity(r.Context(), k, e)
	if err == nil {
		s.relocateEntityDocs(r.Context(), k, id)
		s.touched()
	}
	organizeRedirect(w, r, k, "Saved", err)
}

func (s *Server) uiDeleteEntity(w http.ResponseWriter, r *http.Request) {
	k, err := pathKind(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	ids, err := s.Store.DeleteEntity(r.Context(), k, id)
	if err == nil {
		s.relocate(r.Context(), ids)
		s.touched()
	}
	organizeRedirect(w, r, k, "Deleted", err)
}

// ---- activity, login ----

func (s *Server) uiActivity(w http.ResponseWriter, r *http.Request) {
	tasks := s.Ingester.Tasks(200)
	busy := false
	for _, t := range tasks {
		if t.Status == "queued" || t.Status == "processing" {
			busy = true
		}
	}
	s.render(w, "activity", struct {
		Base
		Tasks      any
		Busy       bool
		Msg        string
		ConsumeDir string
	}{s.base(r.Context(), "Activity", "activity"), tasks, busy, r.URL.Query().Get("msg"), s.ConsumeDir})
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login", struct {
		Base
		Err bool
	}{Base{Title: "Sign in"}, r.URL.Query().Has("err")})
}

func (s *Server) uiLoginPost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if s.Token == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(s.Token)) != 1 {
		time.Sleep(time.Second)
		http.Redirect(w, r, "/login?err", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.sessionValue(), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 90 * 24 * 3600, Secure: r.TLS != nil,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
