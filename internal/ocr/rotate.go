package ocr

import (
	"context"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var (
	rotateRe = regexp.MustCompile(`Rotate:\s*(\d+)`)
	confRe   = regexp.MustCompile(`Orientation confidence:\s*([\d.]+)`)
)

// detectRotation asks tesseract which way a page is turned. It returns the
// clockwise rotation in degrees that makes the page upright, or 0.
func detectRotation(ctx context.Context, img string) int {
	out, err := run(ctx, "tesseract", img, "-", "--psm", "0")
	if err != nil {
		return 0 // usually "too few characters"; leave the page alone
	}
	m, c := rotateRe.FindSubmatch(out), confRe.FindSubmatch(out)
	if m == nil || c == nil {
		return 0
	}
	deg, _ := strconv.Atoi(string(m[1]))
	conf, _ := strconv.ParseFloat(string(c[1]), 64)
	if conf < 2 {
		return 0
	}
	return deg % 360
}

// uprightPage returns a path to an upright version of img (JPEG or PNG),
// or img itself when no rotation is needed or possible.
func uprightPage(ctx context.Context, img, workdir string, i int) string {
	deg := detectRotation(ctx, img)
	if deg == 0 {
		return img
	}
	f, err := os.Open(img)
	if err != nil {
		return img
	}
	src, format, err := image.Decode(f)
	f.Close()
	if err != nil || (format != "jpeg" && format != "png") {
		return img
	}
	dst := rotate(src, deg)
	out := filepath.Join(workdir, "upright-"+strconv.Itoa(i)+"."+format)
	w, err := os.Create(out)
	if err != nil {
		return img
	}
	if format == "png" {
		err = png.Encode(w, dst)
	} else {
		err = jpeg.Encode(w, dst, &jpeg.Options{Quality: 90})
	}
	if cerr := w.Close(); err != nil || cerr != nil {
		return img
	}
	return out
}

// rotate turns an image clockwise by 90, 180 or 270 degrees.
func rotate(src image.Image, deg int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if g, ok := src.(*image.Gray); ok {
		return rotatePix(g.Pix, g.Stride, w, h, 1, deg, func(r image.Rectangle) ([]uint8, int, image.Image) {
			d := image.NewGray(r)
			return d.Pix, d.Stride, d
		})
	}
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	return rotatePix(rgba.Pix, rgba.Stride, w, h, 4, deg, func(r image.Rectangle) ([]uint8, int, image.Image) {
		d := image.NewRGBA(r)
		return d.Pix, d.Stride, d
	})
}

func rotatePix(pix []uint8, stride, w, h, bpp, deg int, alloc func(image.Rectangle) ([]uint8, int, image.Image)) image.Image {
	dw, dh := w, h
	if deg == 90 || deg == 270 {
		dw, dh = h, w
	}
	dpix, dstride, img := alloc(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch deg {
			case 90:
				nx, ny = h-1-y, x
			case 180:
				nx, ny = w-1-x, h-1-y
			case 270:
				nx, ny = y, w-1-x
			default:
				nx, ny = x, y
			}
			copy(dpix[ny*dstride+nx*bpp:ny*dstride+nx*bpp+bpp], pix[y*stride+x*bpp:y*stride+x*bpp+bpp])
		}
	}
	return img
}
