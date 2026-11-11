package ocr

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Office documents (Word, Excel, PowerPoint and their LibreOffice
// counterparts) are ZIP files full of XML. We only need their text, so
// unzipping and reading the XML is enough: no LibreOffice required.

const (
	MimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MimePPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MimeODT  = "application/vnd.oasis.opendocument.text"
	MimeODS  = "application/vnd.oasis.opendocument.spreadsheet"
	MimeODP  = "application/vnd.oasis.opendocument.presentation"
)

var officeExt = map[string]string{
	MimeDOCX: ".docx", MimeXLSX: ".xlsx", MimePPTX: ".pptx",
	MimeODT: ".odt", MimeODS: ".ods", MimeODP: ".odp",
}

// Short names for the UI, e.g. on thumbnails.
var officeLabel = map[string]string{
	MimeDOCX: "DOCX", MimeXLSX: "XLSX", MimePPTX: "PPTX",
	MimeODT: "ODT", MimeODS: "ODS", MimeODP: "ODP",
}

func IsOffice(mime string) bool { _, ok := officeExt[mime]; return ok }

// Label returns a short type name for files without a preview.
func Label(mime string) string {
	if l, ok := officeLabel[mime]; ok {
		return l
	}
	if strings.HasPrefix(mime, "text/") {
		return "TXT"
	}
	return "FILE"
}

// Limits against zip bombs: documents are small, their XML is not huge.
const (
	maxEntry = 64 << 20
	maxText  = 8 << 20
)

// detectZipOffice tells which office format a ZIP file is, or "".
func detectZipOffice(p string) string {
	z, err := zip.OpenReader(p)
	if err != nil {
		return ""
	}
	defer z.Close()
	for _, f := range z.File {
		switch f.Name {
		case "mimetype": // OpenDocument stores its type in plain text
			if b, err := readEntry(f, 200); err == nil {
				m := strings.TrimSpace(string(b))
				if IsOffice(m) {
					return m
				}
			}
		case "word/document.xml":
			return MimeDOCX
		case "xl/workbook.xml":
			return MimeXLSX
		case "ppt/presentation.xml":
			return MimePPTX
		}
	}
	return ""
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// numbered sorts entries like slide2.xml before slide10.xml.
var numRe = regexp.MustCompile(`(\d+)\.xml$`)

func numbered(files []*zip.File, prefix string) []*zip.File {
	var out []*zip.File
	for _, f := range files {
		if strings.HasPrefix(f.Name, prefix) && numRe.MatchString(f.Name) && path.Dir(f.Name)+"/" == path.Dir(prefix)+"/" {
			out = append(out, f)
		}
	}
	n := func(f *zip.File) int { v, _ := strconv.Atoi(numRe.FindStringSubmatch(f.Name)[1]); return v }
	sort.Slice(out, func(i, j int) bool { return n(out[i]) < n(out[j]) })
	return out
}

// xmlText walks an XML entry. Character data is kept only inside elements
// named in textIn (or everywhere if textIn is nil); breaks maps element
// names to what to write when the element ends (or, for empty elements
// like tabs, starts).
type xmlRules struct {
	textIn   map[string]bool
	onStart  map[string]string
	onEnd    map[string]string
	repeatSp string // ODF <text:s text:c="3"/> means three spaces
}

func xmlText(f *zip.File, r xmlRules, b *strings.Builder) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, maxEntry))
	dec.Strict = false
	depth := 0 // how many textIn elements we are inside
	cells := 0 // inside a table cell, paragraphs don't break the row
	for b.Len() < maxText {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if r.textIn[name] {
				depth++
			}
			if name == "tc" || name == "table-cell" {
				cells++
			}
			if s, ok := r.onStart[name]; ok {
				b.WriteString(s)
			}
			if r.repeatSp != "" && name == r.repeatSp {
				n := 1
				for _, a := range t.Attr {
					if a.Name.Local == "c" {
						n, _ = strconv.Atoi(a.Value)
					}
				}
				b.WriteString(strings.Repeat(" ", max(1, min(n, 200))))
			}
		case xml.EndElement:
			name := t.Name.Local
			if r.textIn[name] {
				depth--
			}
			if name == "tc" || name == "table-cell" {
				cells--
			}
			if s, ok := r.onEnd[name]; ok {
				if cells > 0 && (name == "p" || name == "h") {
					s = " "
				}
				b.WriteString(s)
			}
		case xml.CharData:
			if r.textIn == nil || depth > 0 {
				b.Write(t)
			}
		}
	}
	return nil
}

// ExtractOffice returns the text of an office document and a page count.
func ExtractOffice(p, mime string) (string, int, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", 0, err
	}
	defer z.Close()
	files := map[string]*zip.File{}
	for _, f := range z.File {
		files[f.Name] = f
	}
	var b strings.Builder
	pages := 1

	switch mime {
	case MimeDOCX:
		rules := xmlRules{
			textIn:  map[string]bool{"t": true},
			onStart: map[string]string{"tab": "\t", "br": "\n", "cr": "\n"},
			onEnd:   map[string]string{"p": "\n", "tc": "\t", "tr": "\n"},
		}
		// Letterheads often live in the header, so read it first.
		for _, f := range numbered(z.File, "word/header") {
			xmlText(f, rules, &b)
		}
		f := files["word/document.xml"]
		if f == nil {
			return "", 0, errors.New("not a Word document")
		}
		if err := xmlText(f, rules, &b); err != nil {
			return "", 0, err
		}
		for _, f := range numbered(z.File, "word/footer") {
			xmlText(f, rules, &b)
		}
		pages = docxPages(files["docProps/app.xml"])

	case MimePPTX:
		slides := numbered(z.File, "ppt/slides/slide")
		for _, f := range slides {
			xmlText(f, xmlRules{textIn: map[string]bool{"t": true}, onEnd: map[string]string{"p": "\n"}}, &b)
			b.WriteString("\f\n")
		}
		pages = max(1, len(slides))

	case MimeXLSX:
		shared := xlsxShared(files["xl/sharedStrings.xml"])
		dates := xlsxDateStyles(files["xl/styles.xml"])
		sheets := numbered(z.File, "xl/worksheets/sheet")
		for _, f := range sheets {
			if err := xlsxSheet(f, shared, dates, &b); err != nil {
				return "", 0, err
			}
			b.WriteString("\f\n")
		}
		pages = max(1, len(sheets))

	case MimeODT, MimeODS, MimeODP:
		f := files["content.xml"]
		if f == nil {
			return "", 0, errors.New("not an OpenDocument file")
		}
		err := xmlText(f, xmlRules{
			textIn:   map[string]bool{"p": true, "h": true},
			onStart:  map[string]string{"tab": "\t", "line-break": "\n"},
			onEnd:    map[string]string{"p": "\n", "h": "\n", "table-cell": "\t", "table-row": "\n", "page": "\f\n"},
			repeatSp: "s",
		}, &b)
		if err != nil {
			return "", 0, err
		}
		if mime == MimeODP {
			pages = max(1, strings.Count(b.String(), "\f"))
		}

	default:
		return "", 0, fmt.Errorf("%w: %s", ErrUnsupported, mime)
	}
	// Paragraphs inside table cells end with a space; drop it before the tab.
	return CleanText(strings.ReplaceAll(b.String(), " \t", "\t")), pages, nil
}

func docxPages(f *zip.File) int {
	if f == nil {
		return 1
	}
	b, err := readEntry(f, 1<<20)
	if err != nil {
		return 1
	}
	if m := regexp.MustCompile(`<Pages>(\d+)</Pages>`).FindSubmatch(b); m != nil {
		if n, _ := strconv.Atoi(string(m[1])); n > 0 {
			return n
		}
	}
	return 1
}

// xlsxShared reads the workbook's shared string table.
func xlsxShared(f *zip.File) []string {
	if f == nil {
		return nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, maxEntry))
	var out []string
	var cur strings.Builder
	inT := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "si" {
				cur.Reset()
			}
			inT = t.Name.Local == "t" || inT
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				out = append(out, cur.String())
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
}

// xlsxSheet writes a sheet's cells as tab-separated rows.
func xlsxSheet(f *zip.File, shared []string, dates map[int]bool, b *strings.Builder) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, maxEntry))
	var cellType string
	var cellStyle int
	var val strings.Builder
	inV := false
	for b.Len() < maxText {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "c":
				cellType, cellStyle = "", 0
				val.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						cellType = a.Value
					case "s":
						cellStyle, _ = strconv.Atoi(a.Value)
					}
				}
			case "v", "t":
				inV = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inV = false
			case "c":
				v := val.String()
				if cellType == "s" {
					if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(shared) {
						v = shared[i]
					}
				} else if (cellType == "" || cellType == "n") && dates[cellStyle] {
					v = excelDate(v)
				}
				if v != "" {
					b.WriteString(v)
					b.WriteByte('\t')
				}
			case "row":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inV {
				val.Write(t)
			}
		}
	}
	return nil
}

// xlsxDateStyles returns which cell styles show numbers as dates. Excel
// stores dates as day counts; only the style says it's a date.
func xlsxDateStyles(f *zip.File) map[int]bool {
	out := map[int]bool{}
	if f == nil {
		return out
	}
	rc, err := f.Open()
	if err != nil {
		return out
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, maxEntry))
	custom := map[int]string{}
	inXfs, idx := false, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			attr := func(n string) string {
				for _, a := range t.Attr {
					if a.Name.Local == n {
						return a.Value
					}
				}
				return ""
			}
			switch t.Name.Local {
			case "numFmt":
				id, _ := strconv.Atoi(attr("numFmtId"))
				custom[id] = attr("formatCode")
			case "cellXfs":
				inXfs = true
			case "xf":
				if inXfs {
					id, _ := strconv.Atoi(attr("numFmtId"))
					out[idx] = isDateFormat(id, custom[id])
					idx++
				}
			}
		case xml.EndElement:
			if t.Name.Local == "cellXfs" {
				inXfs = false
			}
		}
	}
}

var quotedRe = regexp.MustCompile(`"[^"]*"|\\.|\[[^\]]*\]`)

func isDateFormat(id int, code string) bool {
	if (id >= 14 && id <= 22) || (id >= 45 && id <= 47) {
		return true
	}
	if code == "" {
		return false
	}
	c := strings.ToLower(quotedRe.ReplaceAllString(code, ""))
	return strings.ContainsAny(c, "dy") || (strings.Contains(c, "m") && strings.Contains(c, "h"))
}

// excelDate turns Excel's day count (days since 30 Dec 1899) into YYYY-MM-DD.
func excelDate(v string) string {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 1 || f > 2958465 {
		return v
	}
	t := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).Add(time.Duration(f * 24 * float64(time.Hour)))
	if f != float64(int64(f)) {
		return t.Format("2006-01-02 15:04")
	}
	return t.Format("2006-01-02")
}
