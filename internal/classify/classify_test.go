package classify

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"paperfull/internal/store"
)

func TestClassifyLearnsFromReviewedDocuments(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mk := func(k store.Kind, name, algo, match string) int64 {
		id, err := st.CreateEntity(ctx, k, store.Entity{Name: name, Algorithm: algo, Match: match})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	stadtwerke := mk(store.KindCorrespondent, "Stadtwerke", "auto", "")
	finanzamt := mk(store.KindCorrespondent, "Finanzamt", "any", "Finanzamt")
	rechnung := mk(store.KindDocumentType, "Rechnung", "auto", "")
	bescheid := mk(store.KindDocumentType, "Bescheid", "auto", "")
	strom := mk(store.KindTag, "Strom", "auto", "")
	steuer := mk(store.KindTag, "Steuer", "auto", "")
	wichtig := mk(store.KindTag, "Wichtig", "literal", "Frist")

	add := func(i int, content string, corr, typ int64, tags ...int64) {
		_, err := st.InsertDocument(ctx, store.NewDocument{
			Title: "t", Content: content, Created: "2024-01-01", Checksum: string(rune('a' + i)),
			CorrespondentID: &corr, DocumentTypeID: &typ, TagIDs: tags, Inbox: false,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add(0, "Stadtwerke Musterstadt GmbH Kundennummer 4711 Stromrechnung Verbrauch kWh Gesamtbetrag Bitte überweisen Sie", stadtwerke, rechnung, strom)
	add(1, "Stadtwerke Musterstadt GmbH Kundennummer 4711 Stromrechnung Verbrauch kWh Gesamtbetrag", stadtwerke, rechnung, strom)
	add(2, "Finanzamt Musterstadt Steuernummer Bescheid Einkommensteuer Erstattung", finanzamt, bescheid, steuer)

	c := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.Train(ctx); err != nil {
		t.Fatal(err)
	}

	p, err := c.Classify(ctx, "Stadtwerke Musterstadt GmbH Kundennummer 4711 Stromrechnung Verbrauch 2025 Gesamtbetrag")
	if err != nil {
		t.Fatal(err)
	}
	if p.CorrespondentID == nil || *p.CorrespondentID != stadtwerke || p.DocumentTypeID == nil || *p.DocumentTypeID != rechnung {
		t.Errorf("strom bill: got %+v", p)
	}
	if len(p.TagIDs) != 1 || p.TagIDs[0] != strom {
		t.Errorf("strom bill tags: got %v", p.TagIDs)
	}

	// Rule match wins for the sender; type and tags come from that sender's documents.
	p, _ = c.Classify(ctx, "Finanzamt Musterstadt Anforderung von Unterlagen. Bitte reichen Sie Belege ein. Frist beachten")
	if p.CorrespondentID == nil || *p.CorrespondentID != finanzamt {
		t.Errorf("finanzamt letter correspondent: got %+v", p.CorrespondentID)
	}
	if p.DocumentTypeID == nil || *p.DocumentTypeID != bescheid {
		t.Errorf("finanzamt letter type: got %+v", p.DocumentTypeID)
	}
	if len(p.TagIDs) != 2 || p.TagIDs[0] != steuer || p.TagIDs[1] != wichtig {
		t.Errorf("finanzamt letter tags: got %v, want [%d %d]", p.TagIDs, steuer, wichtig)
	}

	// Something unrelated gets nothing.
	p, _ = c.Classify(ctx, "Lieber Max, herzliche Glückwünsche zum Geburtstag!")
	if p.CorrespondentID != nil || p.DocumentTypeID != nil || len(p.TagIDs) != 0 {
		t.Errorf("unrelated: got %+v", p)
	}
}
