package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMatches(t *testing.T) {
	text := "Stadtwerke Müller GmbH\nRechnung  Nr. 12345"
	cases := []struct {
		e    Entity
		want bool
	}{
		{Entity{Algorithm: "any", Match: "foo müller"}, true},
		{Entity{Algorithm: "any", Match: "mülle"}, false}, // whole words only
		{Entity{Algorithm: "all", Match: "stadtwerke rechnung"}, true},
		{Entity{Algorithm: "all", Match: "stadtwerke gas"}, false},
		{Entity{Algorithm: "all", Match: `"müller gmbh" rechnung`}, true},
		{Entity{Algorithm: "literal", Match: "Rechnung Nr."}, true},
		{Entity{Algorithm: "literal", Match: "rechnung nr.", CaseSensitive: true}, false},
		{Entity{Algorithm: "regex", Match: `Nr\. \d{5}`}, true},
		{Entity{Algorithm: "auto", Match: "stadtwerke"}, false},
		{Entity{Algorithm: "none", Match: "stadtwerke"}, false},
	}
	for _, c := range cases {
		if got := Matches(c.e, text); got != c.want {
			t.Errorf("%s %q: got %v, want %v", c.e.Algorithm, c.e.Match, got, c.want)
		}
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tax, _ := s.CreateEntity(ctx, KindTag, Entity{Name: "Steuer 2023"})
	corr, _ := s.CreateEntity(ctx, KindCorrespondent, Entity{Name: "Stadtwerke"})
	docs := []NewDocument{
		{Title: "Strom", Content: "Stromrechnung über 100 EUR", Created: "2024-03-01", Checksum: "1", CorrespondentID: &corr},
		{Title: "Steuerbescheid", Content: "Einkommensteuer 2023, Erstattung", Created: "2024-08-21", Checksum: "2", TagIDs: []int64{tax}},
		{Title: "Gas", Content: "Gasrechnung Abschlag", Created: "2023-01-10", Checksum: "3", CorrespondentID: &corr, Inbox: true},
	}
	for _, d := range docs {
		if _, err := s.InsertDocument(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		q    string
		want []int64
	}{
		{"rechnung", []int64{1, 3}},
		{"ueber", nil},
		{"uber", []int64{1}}, // diacritics are ignored
		{"rechnung -gas", []int64{1}},
		{"strom OR erstattung", []int64{1, 2}},
		{`tag:"steuer 2023"`, []int64{2}},
		{"-tag:none", []int64{2}},
		{"from:stadtwerke after:2024", []int64{1}},
		{"correspondent:none", []int64{2}},
		{"year:2023", []int64{3}},
		{"before:2024-03-02 after:2024-03", []int64{1}},
		{"inbox:yes", []int64{3}},
		{"title:steuer", []int64{2}},
		{"EU", []int64{1, 2}}, // short terms fall back to substring match ("StEUerbescheid")
		{`"über 100"`, []int64{1}},
		{"id:2", []int64{2}},
		{"stadtwerke", []int64{1, 3}}, // correspondent names are searchable
	}
	for _, c := range cases {
		res, err := s.Search(ctx, SearchOptions{Query: c.q, Sort: "created"})
		if err != nil {
			t.Errorf("%q: %v", c.q, err)
			continue
		}
		got := map[int64]bool{}
		for _, d := range res.Documents {
			got[d.ID] = true
		}
		if len(got) != len(c.want) || res.Total != len(c.want) {
			t.Errorf("%q: got %v (total %d), want %v", c.q, got, res.Total, c.want)
			continue
		}
		for _, id := range c.want {
			if !got[id] {
				t.Errorf("%q: got %v, want %v", c.q, got, c.want)
				break
			}
		}
	}
	if _, err := s.Search(ctx, SearchOptions{Query: "after:soon"}); err == nil {
		t.Error("expected error for bad date")
	}
	// Renaming a tag updates the index.
	if err := s.UpdateEntity(ctx, KindTag, Entity{ID: tax, Name: "Finanzen", Algorithm: "auto"}); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Search(ctx, SearchOptions{Query: "finanzen"})
	if res.Total != 1 {
		t.Errorf("after tag rename: got %d results", res.Total)
	}
}

func TestFindSimilar(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, n := range []string{"Stadtwerke Musterstadt", "Finanzamt", "Allianz"} {
		s.CreateEntity(ctx, KindCorrespondent, Entity{Name: n})
	}
	for _, n := range []string{"Rechnung", "Vertrag", "Brief"} {
		s.CreateEntity(ctx, KindDocumentType, Entity{Name: n})
	}
	senders := map[string]string{
		"Stadtwerke Musterstadt GmbH": "Stadtwerke Musterstadt",
		"Finanzamt Musterstadt":       "Finanzamt",
		"Allianz Versicherungs-AG":    "Allianz",
		"Sicherheit Plus":             "",
		"Musterstadt":                 "",
	}
	for in, want := range senders {
		e, ok := s.FindSimilarEntity(ctx, KindCorrespondent, in)
		if got := map[bool]string{true: "", false: ""}[ok]; ok {
			got = e.Name
			if got != want {
				t.Errorf("sender %q: got %q, want %q", in, got, want)
			}
		} else if want != "" {
			t.Errorf("sender %q: no match, want %q", in, want)
		}
	}
	types := map[string]string{"Stromrechnung": "Rechnung", "Mietvertrag": "Vertrag", "rechnung": "Rechnung", "Bescheid": "", "Anforderung": ""}
	for in, want := range types {
		e, ok := s.FindSimilarType(ctx, in)
		switch {
		case ok && e.Name != want:
			t.Errorf("type %q: got %q, want %q", in, e.Name, want)
		case !ok && want != "":
			t.Errorf("type %q: no match, want %q", in, want)
		}
	}
}
