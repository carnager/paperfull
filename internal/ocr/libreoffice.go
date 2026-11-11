package ocr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// LibreOffice is optional. When it is installed, office files get a real
// PDF with their original layout (for preview, thumbnail and printing).
// Without it they are shown as a simplified preview built by paperfull.

// FindLibreOffice returns the soffice program, or "" if LibreOffice isn't
// installed. On macOS it lives inside the app bundle, not on the PATH.
func FindLibreOffice() string {
	for _, name := range []string{"soffice", "libreoffice"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/LibreOffice.app/Contents/MacOS/soffice",
			filepath.Join(os.Getenv("HOME"), "Applications/LibreOffice.app/Contents/MacOS/soffice"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// LibreOffice can't run twice at once with the same profile, so
// conversions take turns. They are rare enough that this doesn't matter.
var loMu sync.Mutex

// convertWithLibreOffice turns an office file into a PDF inside workdir.
func convertWithLibreOffice(ctx context.Context, cfg Config, src, mime, workdir string) (string, error) {
	loMu.Lock()
	defer loMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	// Copy to a plain name: LibreOffice names its output after the input
	// and picks the import filter from the extension.
	in := filepath.Join(workdir, "document"+ExtFor(mime))
	if err := copyPlain(src, in); err != nil {
		return "", err
	}
	outDir := filepath.Join(workdir, "lo-out")
	profile := cfg.LibreOfficeProfile
	if profile == "" {
		profile = filepath.Join(workdir, "lo-profile")
	}
	args := []string{
		"--headless", "--norestore", "--nolockcheck", "--nodefault", "--nologo",
		// A private profile keeps it away from a LibreOffice the user has open.
		"-env:UserInstallation=" + (&url.URL{Scheme: "file", Path: profile}).String(),
		"--convert-to", "pdf", "--outdir", outDir, in,
	}
	if _, err := run(ctx, cfg.LibreOffice, args...); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("LibreOffice took too long")
		}
		return "", err
	}
	out := filepath.Join(outDir, "document.pdf")
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("LibreOffice produced no PDF")
	}
	return out, nil
}

func copyPlain(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
