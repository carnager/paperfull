package ocr

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func init() {
	// pdfcpu would otherwise create a settings folder in the user's home.
	api.DisableConfigDir()
}

var (
	ErrPasswordRequired = errors.New("this PDF is protected with a password")
	ErrWrongPassword    = errors.New("wrong password")
)

// NeedsPassword reports whether a PDF can only be opened with a password.
// PDFs that merely restrict printing or editing open fine and return false.
func NeedsPassword(ctx context.Context, path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = api.ReadContext(ctx, f, model.NewDefaultConfiguration())
	return isWrongPassword(err)
}

func isWrongPassword(err error) bool {
	return err != nil && (errors.Is(err, pdfcpu.ErrWrongPassword) || strings.Contains(err.Error(), pdfcpu.ErrWrongPassword.Error()))
}

// DecryptPDF writes an unlocked copy of a protected PDF. The password to open
// the file is enough; the owner password isn't needed.
func DecryptPDF(ctx context.Context, in, out, password string) error {
	conf := model.NewDefaultConfiguration()
	conf.UserPW, conf.OwnerPW = password, password
	if err := api.DecryptFile(ctx, in, out, conf); err != nil {
		os.Remove(out)
		if isWrongPassword(err) {
			return ErrWrongPassword
		}
		return err
	}
	return nil
}
