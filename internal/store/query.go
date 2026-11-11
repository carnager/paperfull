package store

import (
	"strings"
)

// QueryToken is one part of a search query: either free text (Key == "")
// or a filter such as tag:Steuer.
type QueryToken struct {
	Key   string // normalized filter key, "" for text
	Value string
	Neg   bool
	Raw   string // the token as it would be written in a query
}

// filterKeys maps accepted spellings to the normalized key.
var filterKeys = map[string]string{
	"tag": "tag", "tags": "tag",
	"from": "from", "sender": "from", "correspondent": "from",
	"type":  "type",
	"after": "after", "before": "before", "year": "year", "created": "created",
	"inbox": "inbox", "id": "id", "title": "title",
}

// SingleValued filters replace each other instead of stacking.
func SingleValued(key string) bool {
	switch key {
	case "from", "type", "after", "before", "year", "created", "inbox", "id":
		return true
	}
	return false
}

func quoteIfNeeded(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\"") {
		return `"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return v
}

// FormatFilter writes a filter the way the query parser reads it.
func FormatFilter(key, value string, neg bool) string {
	p := ""
	if neg {
		p = "-"
	}
	return p + key + ":" + quoteIfNeeded(value)
}

// ParseQuery splits a query into text and filter tokens.
func ParseQuery(q string) []QueryToken {
	var out []QueryToken
	for _, t := range tokenizeQuery(q) {
		if t.colon > 0 {
			if key, ok := filterKeys[strings.ToLower(t.s[:t.colon])]; ok {
				v := t.s[t.colon+1:]
				out = append(out, QueryToken{Key: key, Value: v, Neg: t.neg, Raw: FormatFilter(key, v, t.neg)})
				continue
			}
		}
		raw := t.s
		if t.quoted {
			raw = `"` + strings.ReplaceAll(raw, `"`, "") + `"`
		}
		if t.neg {
			raw = "-" + raw
		}
		out = append(out, QueryToken{Value: t.s, Neg: t.neg, Raw: raw})
	}
	return out
}

// JoinQuery writes tokens back into a query string, filters first.
func JoinQuery(toks []QueryToken) string {
	var f, t []string
	for _, x := range toks {
		if x.Key != "" {
			f = append(f, x.Raw)
		} else {
			t = append(t, x.Raw)
		}
	}
	return strings.TrimSpace(strings.Join(append(f, t...), " "))
}

// WithFilter returns q with the filter added (replacing an earlier value of
// a single-valued filter).
func WithFilter(q, key, value string, neg bool) string {
	var keep []QueryToken
	for _, t := range ParseQuery(q) {
		if t.Key == key && (SingleValued(key) || strings.EqualFold(t.Value, value)) {
			continue
		}
		keep = append(keep, t)
	}
	keep = append(keep, QueryToken{Key: key, Value: value, Neg: neg, Raw: FormatFilter(key, value, neg)})
	return JoinQuery(keep)
}
