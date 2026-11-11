// Package ocr turns PDFs and images into text plus a searchable PDF, using
// poppler-utils (pdftotext, pdftoppm, pdfinfo, pdfunite) and tesseract.
package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type Config struct {
	Languages string // tesseract languages, e.g. "deu+eng"
	DPI       int    // rasterization resolution for scanned PDFs
	Jobs      int    // pages OCR'd in parallel
	Mode      string // "auto": OCR only when a PDF has no usable text layer; "always"
	PSM       int    // tesseract page segmentation mode; 1 = auto with orientation detection
	Rotate    bool   // turn sideways/upside-down pages upright (needs the "osd" language)

	// LibreOffice is the soffice program used to turn office files into
	// PDFs; empty = don't, show a simplified preview instead.
	LibreOffice        string
	LibreOfficeProfile string // its settings folder (kept between runs, faster)
}

type Result struct {
	Text        string
	ArchivePath string // searchable PDF created in the work dir, "" if the original is already fine
	Pages       int
	Mime        string
	Warning     string // something went wrong but the document is still usable
}

var ErrUnsupported = errors.New("unsupported file type")

// Required lists the external programs paperfull needs.
var Required = []string{"tesseract", "pdftotext", "pdftoppm", "pdfinfo", "pdfunite"}

func CheckTools() error {
	var missing []string
	for _, t := range Required {
		if _, err := exec.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing programs: %s (install tesseract and poppler)", strings.Join(missing, ", "))
	}
	return nil
}

// Languages returns the installed tesseract languages.
func Languages() []string {
	out, err := exec.Command("tesseract", "--list-langs").Output()
	if err != nil {
		return nil
	}
	var langs []string
	for _, l := range strings.Split(string(out), "\n")[1:] {
		if l = strings.TrimSpace(l); l != "" {
			langs = append(langs, l)
		}
	}
	return langs
}

func DetectMime(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	buf = buf[:n]
	if bytes.HasPrefix(buf, []byte("II*\x00")) || bytes.HasPrefix(buf, []byte("MM\x00*")) {
		return "image/tiff", nil
	}
	m := http.DetectContentType(buf)
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	if m == "application/zip" {
		if o := detectZipOffice(path); o != "" {
			return o, nil
		}
	}
	if m == "application/octet-stream" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".pdf":
			return "application/pdf", nil
		case ".txt", ".md":
			return "text/plain", nil
		}
	}
	return m, nil
}

func Supported(mime string) bool {
	switch mime {
	case "application/pdf", "image/png", "image/jpeg", "image/tiff", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return strings.HasPrefix(mime, "text/plain") || IsOffice(mime)
}

func ExtFor(mime string) string {
	if e, ok := officeExt[mime]; ok {
		return e
	}
	switch mime {
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/tiff":
		return ".tiff"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	}
	return ".txt"
}

// Process extracts text from src, writing temporary files into workdir.
func Process(ctx context.Context, cfg Config, src, workdir string) (*Result, error) {
	mime, err := DetectMime(src)
	if err != nil {
		return nil, err
	}
	switch {
	case mime == "application/pdf":
		return processPDF(ctx, cfg, src, workdir)
	case strings.HasPrefix(mime, "image/") && Supported(mime):
		return processImage(ctx, cfg, src, workdir, mime)
	case strings.HasPrefix(mime, "text/plain"):
		b, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		return &Result{Text: CleanText(string(b)), Pages: 1, Mime: "text/plain"}, nil
	case IsOffice(mime):
		text, pages, err := ExtractOffice(src, mime)
		if err != nil {
			return nil, fmt.Errorf("reading %s file: %w", Label(mime), err)
		}
		res := &Result{Text: text, Pages: pages, Mime: mime}
		if cfg.LibreOffice != "" {
			pdf, err := convertWithLibreOffice(ctx, cfg, src, mime, workdir)
			if err != nil {
				res.Warning = "no PDF from LibreOffice, using the simplified preview: " + err.Error()
			} else {
				res.ArchivePath = pdf
				if n, err := pdfPages(ctx, pdf); err == nil {
					res.Pages = n
				}
			}
		}
		return res, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, mime)
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Parallelism is done per page; stop tesseract from also spawning threads.
	cmd.Env = append(os.Environ(), "OMP_THREAD_LIMIT=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return stdout.Bytes(), nil
}

var pagesRe = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)

func pdfPages(ctx context.Context, src string) (int, error) {
	out, err := run(ctx, "pdfinfo", src)
	if err != nil {
		return 0, err
	}
	m := pagesRe.FindSubmatch(out)
	if m == nil {
		return 0, errors.New("pdfinfo: page count not found")
	}
	return strconv.Atoi(string(m[1]))
}

// hasTextLayer is true when most pages already carry real text.
func hasTextLayer(text string, pages int) bool {
	parts := strings.Split(text, "\f")
	good := 0
	for _, p := range parts {
		letters := 0
		for _, r := range p {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters >= 30 {
			good++
		}
	}
	return pages > 0 && good*2 > pages
}

func processPDF(ctx context.Context, cfg Config, src, workdir string) (*Result, error) {
	pages, err := pdfPages(ctx, src)
	if err != nil {
		return nil, err
	}
	res := &Result{Pages: pages, Mime: "application/pdf"}
	if cfg.Mode != "always" {
		out, err := run(ctx, "pdftotext", "-layout", "-enc", "UTF-8", src, "-")
		if err == nil && hasTextLayer(string(out), pages) {
			res.Text = CleanText(string(out))
			return res, nil
		}
	}
	prefix := filepath.Join(workdir, "page")
	if _, err := run(ctx, "pdftoppm", "-r", strconv.Itoa(cfg.DPI), "-jpeg", "-jpegopt", "quality=90", src, prefix); err != nil {
		return nil, err
	}
	imgs, err := filepath.Glob(prefix + "-*.jpg")
	if err != nil || len(imgs) == 0 {
		return nil, errors.New("pdftoppm produced no pages")
	}
	sort.Strings(imgs) // pdftoppm zero-pads page numbers
	text, archive, err := ocrPages(ctx, cfg, imgs, workdir, true)
	if err != nil {
		return nil, err
	}
	res.Text, res.ArchivePath = text, archive
	return res, nil
}

func processImage(ctx context.Context, cfg Config, src, workdir, mime string) (*Result, error) {
	text, archive, err := ocrPages(ctx, cfg, []string{src}, workdir, false)
	if err != nil {
		return nil, err
	}
	pages := 1
	if archive != "" {
		if n, err := pdfPages(ctx, archive); err == nil {
			pages = n
		}
	}
	return &Result{Text: text, ArchivePath: archive, Pages: pages, Mime: mime}, nil
}

func tesseractArgs(cfg Config, in, outBase string, setDPI bool) []string {
	args := []string{in, outBase, "-l", cfg.Languages, "--psm", strconv.Itoa(cfg.PSM)}
	if setDPI {
		args = append(args, "--dpi", strconv.Itoa(cfg.DPI))
	}
	return append(args, "pdf", "txt")
}

// ocrPages runs tesseract on each image in parallel and merges the per-page
// searchable PDFs into one.
func ocrPages(ctx context.Context, cfg Config, imgs []string, workdir string, fromPDF bool) (string, string, error) {
	jobs := max(cfg.Jobs, 1)
	texts := make([]string, len(imgs))
	pdfs := make([]string, len(imgs))
	errs := make([]error, len(imgs))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, img := range imgs {
		wg.Add(1)
		go func(i int, img string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			base := filepath.Join(workdir, fmt.Sprintf("ocr-%05d", i))
			if cfg.Rotate {
				img = uprightPage(ctx, img, workdir, i)
			}
			if _, err := run(ctx, "tesseract", tesseractArgs(cfg, img, base, fromPDF)...); err != nil {
				errs[i] = err
				return
			}
			b, err := os.ReadFile(base + ".txt")
			if err != nil {
				errs[i] = err
				return
			}
			texts[i] = strings.TrimRight(string(b), "\f\n ")
			pdfs[i] = base + ".pdf"
		}(i, img)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return "", "", err
	}
	archive := filepath.Join(workdir, "archive.pdf")
	if len(pdfs) == 1 {
		if err := os.Rename(pdfs[0], archive); err != nil {
			return "", "", err
		}
	} else {
		if _, err := run(ctx, "pdfunite", append(pdfs, archive)...); err != nil {
			return "", "", err
		}
	}
	return CleanText(strings.Join(texts, "\n\f\n")), archive, nil
}

// Thumbnail renders the first page of a PDF as a JPEG at dst.
func Thumbnail(ctx context.Context, pdf, dst string) error {
	base := strings.TrimSuffix(dst, filepath.Ext(dst))
	if _, err := run(ctx, "pdftoppm", "-f", "1", "-l", "1", "-scale-to", "480", "-jpeg", "-singlefile", pdf, base); err != nil {
		return err
	}
	if base+".jpg" != dst {
		return os.Rename(base+".jpg", dst)
	}
	return nil
}

var multiBlank = regexp.MustCompile(`\n{3,}`)

func CleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(multiBlank.ReplaceAllString(s, "\n\n"))
}
