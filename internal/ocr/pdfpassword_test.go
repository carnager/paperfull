package ocr

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

const samplePDF = "../../samples/Stromrechnung-2025.pdf"

// LockPDF writes a copy of in that needs userPW to open.
func lockPDF(t *testing.T, userPW, ownerPW string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "locked.pdf")
	if err := api.EncryptFile(context.Background(), samplePDF, out, model.NewAESConfiguration(userPW, ownerPW, 256)); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPasswordPDFs(t *testing.T) {
	ctx := context.Background()
	if NeedsPassword(ctx, samplePDF) {
		t.Fatal("a normal PDF doesn't need a password")
	}
	// Only restricted (printing etc.), opens without a password.
	if NeedsPassword(ctx, lockPDF(t, "", "owner-secret")) {
		t.Error("a PDF with only an owner password opens without asking")
	}

	locked := lockPDF(t, "geheim", "owner-secret")
	if !NeedsPassword(ctx, locked) {
		t.Fatal("locked PDF not recognised")
	}
	out := filepath.Join(t.TempDir(), "open.pdf")
	if err := DecryptPDF(ctx, locked, out, "falsch"); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("wrong password: got %v", err)
	}
	// The password to open is enough, the owner password isn't needed.
	if err := DecryptPDF(ctx, locked, out, "geheim"); err != nil {
		t.Fatal(err)
	}
	if NeedsPassword(ctx, out) {
		t.Error("unlocked copy still needs a password")
	}
}

func TestProcessRefusesLockedPDF(t *testing.T) {
	_, err := Process(context.Background(), Config{}, lockPDF(t, "geheim", "geheim"), t.TempDir())
	if !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("got %v, want ErrPasswordRequired", err)
	}
}

func TestUnlockedPDFIsReadable(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("poppler not installed")
	}
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "open.pdf")
	if err := DecryptPDF(ctx, lockPDF(t, "geheim", "x"), out, "geheim"); err != nil {
		t.Fatal(err)
	}
	if _, err := pdfPages(ctx, out); err != nil {
		t.Errorf("poppler can't read the unlocked copy: %v", err)
	}
}
