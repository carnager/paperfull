package store

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Query syntax (all parts optional, combined with AND):
//
//	words            full-text search, matches inside words too ("rechnung" finds "Stromrechnung")
//	"exact phrase"   phrase search
//	a OR b           either term
//	-word            exclude
//	title:word       search only in titles
//	tag:name         has tag (tag:none = no tags); -tag:name excludes
//	correspondent:x  alias from:x (correspondent:none = unset)
//	type:x           document type (type:none = unset)
//	after:2023-05    created on or after (YYYY, YYYY-MM or YYYY-MM-DD)
//	before:2024      created before
//	year:2023        created in that year
//	inbox:yes|no     still in inbox / reviewed
//	id:42            a single document
type SearchOptions struct {
	Query  string
	Sort   string // relevance, created, -created, added, -added, title, -title
	Limit  int
	Offset int
}

type SearchResult struct {
	Total     int         `json:"total"`
	Documents []*Document `json:"documents"`
}

type rawTok struct {
	s      string
	neg    bool
	colon  int // index of first ':' before any quote, -1 if none
	quoted bool
}

func tokenizeQuery(q string) []rawTok {
	var toks []rawTok
	var cur strings.Builder
	t := rawTok{colon: -1}
	inQuote, started := false, false
	flush := func() {
		if cur.Len() > 0 || t.quoted {
			t.s = cur.String()
			toks = append(toks, t)
		}
		cur.Reset()
		t = rawTok{colon: -1}
		started = false
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			t.quoted = true
			started = true
		case !inQuote && unicode.IsSpace(r):
			flush()
		case !inQuote && !started && r == '-':
			t.neg = true
			started = true
		case !inQuote && r == ':' && t.colon < 0 && !t.quoted:
			t.colon = cur.Len()
			cur.WriteRune(r)
			started = true
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return toks
}

var dateRe = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2})?)?$`)

func lowerDate(v string) (string, bool) {
	if !dateRe.MatchString(v) {
		return "", false
	}
	switch len(v) {
	case 4:
		return v + "-01-01", true
	case 7:
		return v + "-01", true
	}
	return v, true
}

func ftsQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func truthy(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "yes", "true", "1", "y", "on":
		return true, true
	case "no", "false", "0", "n", "off":
		return false, true
	}
	return false, false
}

type builtQuery struct {
	ftsExpr string
	where   []string
	args    []any
}

func buildQuery(q string) (builtQuery, error) {
	var b builtQuery
	var groups [][]string // OR groups, AND-ed together
	orNext := false
	add := func(cond string, neg bool, args ...any) {
		if neg {
			cond = "NOT (" + cond + ")"
		}
		b.where = append(b.where, cond)
		b.args = append(b.args, args...)
	}
	addText := func(expr string, neg bool) {
		if neg {
			add(`d.id IN (SELECT rowid FROM docs_fts WHERE docs_fts MATCH ?)`, true, expr)
			return
		}
		if orNext && len(groups) > 0 {
			groups[len(groups)-1] = append(groups[len(groups)-1], expr)
		} else {
			groups = append(groups, []string{expr})
		}
	}

	for _, t := range tokenizeQuery(q) {
		if !t.quoted && !t.neg && t.s == "OR" {
			orNext = true
			continue
		}
		key, val := "", t.s
		if t.colon > 0 {
			key = strings.ToLower(t.s[:t.colon])
			val = t.s[t.colon+1:]
		}
		switch key {
		case "tag", "tags":
			if strings.EqualFold(val, "none") {
				add(`NOT EXISTS (SELECT 1 FROM document_tags dt WHERE dt.document_id = d.id)`, t.neg)
			} else {
				add(`EXISTS (SELECT 1 FROM document_tags dt JOIN tags g ON g.id = dt.tag_id
				  WHERE dt.document_id = d.id AND g.name = ? COLLATE NOCASE)`, t.neg, val)
			}
		case "correspondent", "from", "sender":
			if strings.EqualFold(val, "none") {
				add(`d.correspondent_id IS NULL`, t.neg)
			} else {
				add(`IFNULL(c.name,'') = ? COLLATE NOCASE`, t.neg, val)
			}
		case "type":
			if strings.EqualFold(val, "none") {
				add(`d.document_type_id IS NULL`, t.neg)
			} else {
				add(`IFNULL(t.name,'') = ? COLLATE NOCASE`, t.neg, val)
			}
		case "after", "before", "year", "created":
			lo, ok := lowerDate(val)
			if !ok {
				return b, fmt.Errorf("%s: expected YYYY, YYYY-MM or YYYY-MM-DD, got %q", key, val)
			}
			switch key {
			case "after":
				add(`d.created >= ?`, t.neg, lo)
			case "before":
				add(`d.created < ?`, t.neg, lo)
			default:
				add(`d.created LIKE ?`, t.neg, val+"%")
			}
		case "inbox":
			v, ok := truthy(val)
			if !ok {
				return b, fmt.Errorf("inbox: expected yes or no, got %q", val)
			}
			add(`d.inbox = ?`, t.neg, boolInt(v))
		case "id":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return b, fmt.Errorf("id: %q is not a number", val)
			}
			add(`d.id = ?`, t.neg, n)
		case "title":
			if utf8.RuneCountInString(val) < 3 {
				add(`d.title LIKE ? ESCAPE '\'`, t.neg, likeEscape(val))
			} else {
				addText("title : "+ftsQuote(val), t.neg)
			}
		default:
			val = t.s
			if strings.TrimSpace(val) == "" {
				break
			}
			if utf8.RuneCountInString(val) < 3 {
				// The trigram index can't match fewer than 3 characters.
				add(`(d.title LIKE ? ESCAPE '\' OR d.content LIKE ? ESCAPE '\')`, t.neg, likeEscape(val), likeEscape(val))
			} else {
				addText(ftsQuote(val), t.neg)
			}
		}
		orNext = false
	}
	var parts []string
	for _, g := range groups {
		if len(g) == 1 {
			parts = append(parts, g[0])
		} else {
			parts = append(parts, "("+strings.Join(g, " OR ")+")")
		}
	}
	b.ftsExpr = strings.Join(parts, " AND ")
	return b, nil
}

func (s *Store) Search(ctx context.Context, o SearchOptions) (*SearchResult, error) {
	bq, err := buildQuery(o.Query)
	if err != nil {
		return nil, err
	}
	if o.Limit <= 0 || o.Limit > 500 {
		o.Limit = 50
	}
	from := docJoins
	var where []string
	var args []any
	snippet := `''`
	if bq.ftsExpr != "" {
		from += ` JOIN docs_fts ON docs_fts.rowid = d.id`
		where = append(where, `docs_fts MATCH ?`)
		args = append(args, bq.ftsExpr)
		snippet = `highlight(docs_fts, 1, char(1), char(2))`
	}
	where = append(where, bq.where...)
	args = append(args, bq.args...)
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	var res SearchResult
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+cond, args...).Scan(&res.Total); err != nil {
		return nil, friendlyFTSError(err)
	}

	order := `d.created DESC, d.id DESC`
	switch o.Sort {
	case "", "relevance":
		if bq.ftsExpr != "" {
			order = `bm25(docs_fts, 10.0, 1.0, 5.0), d.created DESC`
		}
	case "created":
		order = `d.created, d.id`
	case "added":
		order = `d.added, d.id`
	case "-added":
		order = `d.added DESC, d.id DESC`
	case "title":
		order = `d.title COLLATE NOCASE, d.id`
	case "-title":
		order = `d.title COLLATE NOCASE DESC, d.id DESC`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+docCols+`, `+snippet+from+cond+
		` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, o.Limit, o.Offset)...)
	if err != nil {
		return nil, friendlyFTSError(err)
	}
	defer rows.Close()
	res.Documents = []*Document{}
	for rows.Next() {
		var snip string
		d, err := scanDoc(rows, &snip)
		if err != nil {
			return nil, err
		}
		d.Snippet = renderSnippet(snip)
		res.Documents = append(res.Documents, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return &res, s.loadTags(ctx, res.Documents)
}

func friendlyFTSError(err error) error {
	if err != nil && strings.Contains(err.Error(), "fts5") {
		return fmt.Errorf("could not understand search query: %w", err)
	}
	return err
}

// renderSnippet cuts a window around the first hit out of the highlighted
// content and turns the markers into <mark> tags. (FTS5's snippet() counts
// trigram tokens, i.e. single characters, so its snippets are too short.)
func renderSnippet(s string) string {
	start := strings.IndexByte(s, '\x01')
	if start < 0 {
		return ""
	}
	r := []rune(s)
	pos := len([]rune(s[:start]))
	from := max(0, pos-80)
	to := min(len(r), pos+220)
	// Don't cut through a highlighted hit.
	for to < len(r) && strings.Count(string(r[from:to]), "\x01") > strings.Count(string(r[from:to]), "\x02") {
		to++
	}
	out := string(r[from:to])
	out = strings.ReplaceAll(out, "\x02", "\x03") // keep markers distinct while cleaning
	out = strings.Join(strings.Fields(out), " ")
	out = html.EscapeString(out)
	out = strings.ReplaceAll(out, "\x01", "<mark>")
	out = strings.ReplaceAll(out, "\x03", "</mark>")
	if from > 0 {
		out = "…" + out
	}
	if to < len(r) {
		out += "…"
	}
	return out
}
