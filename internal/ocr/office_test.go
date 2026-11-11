package ocr

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExcelDate(t *testing.T) {
	cases := map[string]string{"46027": "2026-01-05", "45292.5": "2024-01-01 12:00", "abc": "abc", "0": "0"}
	for in, want := range cases {
		if got := excelDate(in); got != want {
			t.Errorf("excelDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDateFormat(t *testing.T) {
	cases := []struct {
		id   int
		code string
		want bool
	}{
		{14, "", true}, {164, `yyyy\-mm\-dd`, true}, {165, `dd.mm.yyyy`, true},
		{166, `#,##0.00 "EUR"`, false}, {167, `[Red]0.00`, false}, {168, `0.00 "days"`, false}, {2, "", false},
	}
	for _, c := range cases {
		if got := isDateFormat(c.id, c.code); got != c.want {
			t.Errorf("isDateFormat(%d, %q) = %v, want %v", c.id, c.code, got, c.want)
		}
	}
}

func TestColIndex(t *testing.T) {
	for ref, want := range map[string]int{"A1": 0, "B7": 1, "Z3": 25, "AA10": 26, "AB1": 27} {
		if got := colIndex(ref); got != want {
			t.Errorf("colIndex(%q) = %d, want %d", ref, got, want)
		}
	}
}

const sampleDOCX = "../../samples/Beitragsanpassung-Hausrat.docx"

func TestDetectAndExtractDOCX(t *testing.T) {
	mime, err := DetectMime(sampleDOCX)
	if err != nil || mime != MimeDOCX {
		t.Fatalf("detected %q (%v), want Word", mime, err)
	}
	text, pages, err := ExtractOffice(sampleDOCX, mime)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Sicherheit Plus Versicherung AG", "Beitragsanpassung Ihrer Hausratversicherung", "Jahresbeitrag\t153,60 €\t165,70 €", "Sonderkündigungsrecht"} {
		if !strings.Contains(text, want) {
			t.Errorf("text misses %q", want)
		}
	}
	if pages < 1 {
		t.Errorf("pages = %d", pages)
	}
}

func TestRenderDOCX(t *testing.T) {
	html, err := RenderOfficeHTML(sampleDOCX, MimeDOCX)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h2>Beitragsanpassung Ihrer Hausratversicherung</h2>", "<table>", "<strong>Jahresbeitrag</strong>", `<img alt=""`, "data:image/png;base64,", "<em>Sonderkündigungsrecht</em>"} {
		if !strings.Contains(html, want) {
			t.Errorf("preview misses %q", want)
		}
	}
	if strings.Contains(html, "<script") {
		t.Error("preview must never contain scripts")
	}
}

func TestLibreOfficeConversion(t *testing.T) {
	lo := FindLibreOffice()
	if lo == "" {
		t.Skip("LibreOffice not installed")
	}
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("poppler not installed")
	}
	work := t.TempDir()
	res, err := Process(context.Background(), Config{LibreOffice: lo, LibreOfficeProfile: filepath.Join(t.TempDir(), "profile")}, sampleDOCX, work)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning != "" || res.ArchivePath == "" {
		t.Fatalf("no PDF: %q", res.Warning)
	}
	if !strings.Contains(res.Text, "Jahresbeitrag") || res.Pages < 1 {
		t.Errorf("text or pages wrong: %d pages", res.Pages)
	}
}

func TestBrokenLibreOfficeFallsBack(t *testing.T) {
	res, err := Process(context.Background(), Config{LibreOffice: "/nonexistent/soffice"}, sampleDOCX, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.ArchivePath != "" || res.Warning == "" || !strings.Contains(res.Text, "Jahresbeitrag") {
		t.Errorf("want text, no PDF and a warning; got archive %q warning %q", res.ArchivePath, res.Warning)
	}
}
