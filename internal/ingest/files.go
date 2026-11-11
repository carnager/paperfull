package ingest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"paperfull/internal/store"
)

// DefaultFilenameFormat sorts files into folders per correspondent and year.
const DefaultFilenameFormat = "{correspondent}/{year}/{created} {title}"

// renderPath fills the filename format for a document. The result is a
// relative path without extension.
func (in *Ingester) renderPath(d *store.Document) string {
	format := in.cfg.FilenameFormat
	if format == "" {
		format = DefaultFilenameFormat
	}
	name := func(r *store.Ref) string {
		if r == nil {
			return "none"
		}
		return r.Name
	}
	created := d.Created
	year, month, day := "0000", "00", "00"
	if len(created) == 10 {
		year, month, day = created[:4], created[5:7], created[8:10]
	}
	tags := make([]string, len(d.Tags))
	for i, t := range d.Tags {
		tags[i] = t.Name
	}
	tagStr := strings.Join(tags, ",")
	if tagStr == "" {
		tagStr = "none"
	}
	vals := map[string]string{
		"id":            fmt.Sprintf("%06d", d.ID),
		"title":         d.Title,
		"correspondent": name(d.Correspondent),
		"type":          name(d.DocumentType),
		"document_type": name(d.DocumentType),
		"created":       created,
		"year":          year,
		"month":         month,
		"day":           day,
		"tags":          tagStr,
		"original_name": strings.TrimSuffix(d.OriginalName, filepath.Ext(d.OriginalName)),
	}
	var segs []string
	for _, seg := range strings.Split(format, "/") {
		var b strings.Builder
		for {
			i := strings.IndexByte(seg, '{')
			if i < 0 {
				b.WriteString(seg)
				break
			}
			j := strings.IndexByte(seg[i:], '}')
			if j < 0 {
				b.WriteString(seg)
				break
			}
			b.WriteString(seg[:i])
			key := seg[i+1 : i+j]
			if v, ok := vals[key]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(seg[i : i+j+1])
			}
			seg = seg[i+j+1:]
		}
		if s := sanitize(b.String()); s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return fmt.Sprintf("%06d", d.ID)
	}
	return filepath.Join(segs...)
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 32, strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " .")
	for len(s) > 120 {
		r := []rune(s)
		s = string(r[:len(r)-1])
	}
	return s
}

// uniquePath picks base+ext, adding " (2)", " (3)", … when another file is in
// the way. current is the file's present path, which counts as free.
func uniquePath(root, base, ext, current string) string {
	for i := 1; ; i++ {
		cand := base + ext
		if i > 1 {
			cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		if cand == current {
			return cand
		}
		if _, err := os.Lstat(filepath.Join(root, cand)); os.IsNotExist(err) {
			return cand
		}
	}
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func removeAndPrune(root, rel string) {
	os.Remove(filepath.Join(root, rel))
	pruneDirs(root, filepath.Dir(rel))
}

// pruneDirs removes empty directories from rel upwards, stopping at root.
func pruneDirs(root, rel string) {
	for rel != "." && rel != "" && rel != string(filepath.Separator) {
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			return
		}
		rel = filepath.Dir(rel)
	}
}
