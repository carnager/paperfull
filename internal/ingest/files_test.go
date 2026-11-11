package ingest

import (
	"testing"

	"paperfull/internal/store"
)

func TestRenderPath(t *testing.T) {
	in := &Ingester{cfg: Config{}}
	d := &store.Document{ID: 7, Title: "Rechnung 03/2024: Strom?", Created: "2024-03-14",
		Correspondent: &store.Ref{Name: "Stadtwerke"}, OriginalName: "scan.pdf"}
	if got, want := in.renderPath(d), "Stadtwerke/2024/2024-03-14 Rechnung 03_2024_ Strom_"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	in.cfg.FilenameFormat = "{type}/{id}-{unknown}"
	if got, want := in.renderPath(d), "none/000007-{unknown}"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	d.Correspondent.Name = ".."
	in.cfg.FilenameFormat = "{correspondent}/{title}"
	if got, want := in.renderPath(d), "Rechnung 03_2024_ Strom_"; got != want {
		t.Errorf("path traversal: got %q, want %q", got, want)
	}
}
