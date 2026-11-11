package store

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

func compileRegex(expr string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// splitMatch splits a match string into words, keeping "quoted phrases" together.
func splitMatch(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case r == '"':
			flush()
			inQuote = !inQuote
		case !inQuote && unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// containsWord reports whether word occurs in text bounded by non-alphanumerics.
func containsWord(text, word string) bool {
	if word == "" {
		return false
	}
	for start := 0; ; {
		i := strings.Index(text[start:], word)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(word)
		before := i == 0 || !isAlnum(lastRune(text[:i]))
		after := end == len(text) || !isAlnum(firstRune(text[end:]))
		if before && after {
			return true
		}
		start = i + 1
	}
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// Matches applies the entity's rule to the document text. "auto" and "none"
// never match here; "auto" is handled by the learned classifier.
func Matches(e Entity, text string) bool {
	if strings.TrimSpace(e.Match) == "" {
		return false
	}
	switch e.Algorithm {
	case "regex":
		re, err := compileRegex(e.Match, e.CaseSensitive)
		return err == nil && re.MatchString(text)
	}
	t := normalizeSpace(text)
	m := e.Match
	if !e.CaseSensitive {
		t = strings.ToLower(t)
		m = strings.ToLower(m)
	}
	switch e.Algorithm {
	case "literal":
		return strings.Contains(t, normalizeSpace(m))
	case "any":
		for _, w := range splitMatch(m) {
			if containsWord(t, normalizeSpace(w)) {
				return true
			}
		}
		return false
	case "all":
		words := splitMatch(m)
		for _, w := range words {
			if !containsWord(t, normalizeSpace(w)) {
				return false
			}
		}
		return len(words) > 0
	}
	return false
}

// Legal forms and filler that don't help tell organisations apart.
var nameNoise = map[string]bool{
	"gmbh": true, "mbh": true, "ag": true, "kg": true, "kgaa": true, "ohg": true, "gbr": true, "ug": true,
	"haftungsbeschränkt": true, "co": true, "ev": true, "e": true, "v": true, "se": true, "eg": true,
	"ltd": true, "limited": true, "inc": true, "llc": true, "plc": true, "corp": true, "sa": true, "sarl": true,
	"bv": true, "nv": true, "the": true, "und": true, "and": true,
}

// NormalizeName reduces a name to its distinctive words for comparison.
func NormalizeName(s string) string {
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isAlnum(r) }) {
		if !nameNoise[w] {
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

// FindSimilarEntity returns an existing entity that names the same thing,
// e.g. "Stadtwerke Musterstadt GmbH" for "Stadtwerke Musterstadt".
func (s *Store) FindSimilarEntity(ctx context.Context, k Kind, name string) (*Entity, bool) {
	want := NormalizeName(name)
	if want == "" {
		return nil, false
	}
	es, err := s.ListEntities(ctx, k)
	if err != nil {
		return nil, false
	}
	var best *Entity
	for i := range es {
		have := NormalizeName(es[i].Name)
		if have == "" {
			continue
		}
		if have == want {
			return &es[i], true
		}
		// A known short name inside the longer one ("Finanzamt" in "Finanzamt
		// Musterstadt", "Telekom" in "Deutsche Telekom"), or a shorter name at
		// the start of a known one ("Allianz" for "Allianz Versicherung").
		// Not the other way round: "Musterstadt" is a town, not the Stadtwerke.
		if containsWord(want, have) || strings.HasPrefix(have, want+" ") {
			if best == nil || len(NormalizeName(best.Name)) < len(have) {
				best = &es[i]
			}
		}
	}
	return best, best != nil
}

// FindSimilarType is FindSimilarEntity for document types, which also
// understands German compound words: "Stromrechnung" and "Jahresrechnung"
// are a "Rechnung", "Mietvertrag" is a "Vertrag".
func (s *Store) FindSimilarType(ctx context.Context, name string) (*Entity, bool) {
	if e, ok := s.FindSimilarEntity(ctx, KindDocumentType, name); ok {
		return e, true
	}
	want := NormalizeName(name)
	es, err := s.ListEntities(ctx, KindDocumentType)
	if err != nil || want == "" {
		return nil, false
	}
	var best *Entity
	for i := range es {
		have := NormalizeName(es[i].Name)
		if len([]rune(have)) >= 4 && strings.HasSuffix(want, have) && (best == nil || len(have) > len(NormalizeName(best.Name))) {
			best = &es[i]
		}
	}
	return best, best != nil
}
