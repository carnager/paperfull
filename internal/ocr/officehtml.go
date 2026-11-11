package ocr

import (
	"archive/zip"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"path"
	"strconv"
	"strings"
)

// RenderOfficeHTML turns an office document into simple, safe HTML for the
// preview: headings, bold/italic/underline, lists, tables, images, one
// table per spreadsheet sheet and one card per slide. It is not a layout
// engine; the goal is a readable document, not a pixel-perfect copy.
// All text is escaped and images are inlined as data URIs, so the result
// can be shown with a strict Content-Security-Policy.
func RenderOfficeHTML(p, mime string) (string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", err
	}
	defer z.Close()
	d := &officeDoc{files: map[string]*zip.File{}}
	for _, f := range z.File {
		d.files[f.Name] = f
	}
	var b strings.Builder
	switch mime {
	case MimeDOCX:
		err = d.renderDOCX(&b)
	case MimeXLSX:
		err = d.renderXLSX(&b)
	case MimePPTX:
		err = d.renderPPTX(&b)
	case MimeODT, MimeODP:
		err = d.renderODF(&b, mime == MimeODP)
	case MimeODS:
		err = d.renderODS(&b)
	default:
		err = fmt.Errorf("%w: %s", ErrUnsupported, mime)
	}
	return b.String(), err
}

type officeDoc struct {
	files    map[string]*zip.File
	imgBytes int // budget for inlined images
}

const maxImages = 20 << 20

// ---------- shared helpers ----------

// sink collects HTML for an element whose opening tag depends on
// properties that only appear inside it (paragraph style, cell span…).
type frame struct {
	name    string
	buf     strings.Builder
	tag     string
	class   string
	style   string
	colspan int
	title   bool
	fmt     runFmt
}

type sinks struct {
	stack []*frame
	root  *strings.Builder
}

func (s *sinks) out() *strings.Builder {
	if n := len(s.stack); n > 0 {
		return &s.stack[n-1].buf
	}
	return s.root
}

func (s *sinks) push(name string) *frame {
	f := &frame{name: name}
	s.stack = append(s.stack, f)
	return f
}

// top returns the innermost frame with that name.
func (s *sinks) top(name string) *frame {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if s.stack[i].name == name {
			return s.stack[i]
		}
	}
	return nil
}

func (s *sinks) pop(name string) *frame {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if s.stack[i].name == name {
			f := s.stack[i]
			// Close anything left open inside it (broken files).
			for j := len(s.stack) - 1; j > i; j-- {
				f.buf.WriteString(s.stack[j].buf.String())
			}
			s.stack = s.stack[:i]
			return f
		}
	}
	return nil
}

type runFmt struct{ b, i, u, s bool }

func (r runFmt) wrap(text string) string {
	if r.s {
		text = "<s>" + text + "</s>"
	}
	if r.u {
		text = "<u>" + text + "</u>"
	}
	if r.i {
		text = "<em>" + text + "</em>"
	}
	if r.b {
		text = "<strong>" + text + "</strong>"
	}
	return text
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func on(v string) bool { return v != "0" && v != "false" && v != "none" && v != "off" }

func (d *officeDoc) decoder(name string) (*xml.Decoder, func(), error) {
	f := d.files[name]
	if f == nil {
		return nil, nil, fmt.Errorf("%s missing", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, nil, err
	}
	dec := xml.NewDecoder(io.LimitReader(rc, maxEntry))
	dec.Strict = false
	return dec, func() { rc.Close() }, nil
}

// rels reads a relationships part: id -> path inside the zip.
func (d *officeDoc) rels(part string) map[string]string {
	dir := path.Dir(part)
	relsName := path.Join(dir, "_rels", path.Base(part)+".rels")
	out := map[string]string{}
	dec, done, err := d.decoder(relsName)
	if err != nil {
		return out
	}
	defer done()
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		if t, ok := tok.(xml.StartElement); ok && t.Name.Local == "Relationship" {
			target := attr(t, "Target")
			if attr(t, "TargetMode") == "External" {
				continue
			}
			if strings.HasPrefix(target, "/") {
				out[attr(t, "Id")] = strings.TrimPrefix(target, "/")
			} else {
				out[attr(t, "Id")] = path.Clean(path.Join(dir, target))
			}
		}
	}
}

var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".bmp": "image/bmp", ".webp": "image/webp",
}

// image returns an <img> tag with the picture inlined, or "" if it can't
// be shown in a browser (e.g. EMF) or the budget is used up.
func (d *officeDoc) image(name string, widthPx int) string {
	f := d.files[name]
	mt, ok := imageTypes[strings.ToLower(path.Ext(name))]
	if f == nil || !ok || int(f.UncompressedSize64) > 8<<20 || d.imgBytes+int(f.UncompressedSize64) > maxImages {
		return ""
	}
	data, err := readEntry(f, 8<<20)
	if err != nil {
		return ""
	}
	d.imgBytes += len(data)
	style := ""
	if widthPx > 0 {
		style = fmt.Sprintf(` style="width:%dpx"`, min(widthPx, 2000))
	}
	return `<img alt=""` + style + ` src="data:` + mt + `;base64,` + base64.StdEncoding.EncodeToString(data) + `">`
}

// EMU are the units Office uses for sizes; 9525 EMU = 1 CSS pixel.
func emuToPx(v string) int {
	n, _ := strconv.Atoi(v)
	return n / 9525
}

func alignStyle(v string) string {
	switch v {
	case "center":
		return "text-align:center"
	case "right", "end":
		return "text-align:right"
	case "both", "justify", "distribute":
		return "text-align:justify"
	}
	return ""
}

func closeParagraph(f *frame) string {
	tag := f.tag
	if tag == "" {
		tag = "p"
	}
	content := f.buf.String()
	if strings.TrimSpace(stripTags(content)) == "" && !strings.Contains(content, "<img") {
		return `<p class="blank"></p>`
	}
	open := "<" + tag
	if f.class != "" {
		open += ` class="` + f.class + `"`
	}
	if f.style != "" {
		open += ` style="` + f.style + `"`
	}
	return open + ">" + content + "</" + tag + ">"
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cellOpen(f *frame) string {
	if f.colspan > 1 {
		return fmt.Sprintf(`<td colspan="%d">`, min(f.colspan, 100))
	}
	return "<td>"
}

// ---------- Word (.docx) ----------

func (d *officeDoc) docxStyles() map[string]string {
	out := map[string]string{}
	dec, done, err := d.decoder("word/styles.xml")
	if err != nil {
		return out
	}
	defer done()
	cur := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		if t, ok := tok.(xml.StartElement); ok {
			switch t.Name.Local {
			case "style":
				cur = attr(t, "styleId")
			case "name":
				if cur != "" {
					out[cur] = strings.ToLower(attr(t, "val"))
				}
			}
		}
	}
}

// headingTag maps Word style names to HTML headings. Built-in style names
// are English ("heading 1") whatever the language of Word.
func headingTag(style string) string {
	s := strings.ToLower(style)
	switch {
	case s == "title" || s == "titel":
		return "h1"
	case strings.HasPrefix(s, "heading ") || strings.HasPrefix(s, "überschrift "):
		n, _ := strconv.Atoi(s[strings.LastIndexByte(s, ' ')+1:])
		return "h" + strconv.Itoa(min(max(n+1, 2), 5))
	case s == "subtitle" || s == "untertitel":
		return "h3"
	}
	return ""
}

func (d *officeDoc) renderDOCX(b *strings.Builder) error {
	if d.files["word/document.xml"] == nil {
		return errors.New("not a Word document")
	}
	styles := d.docxStyles()
	for _, f := range numbered(zipList(d.files), "word/header") {
		var h strings.Builder
		d.renderDOCXPart(f.Name, styles, &h)
		if strings.TrimSpace(stripTags(h.String())) != "" || strings.Contains(h.String(), "<img") {
			b.WriteString(`<header class="docheader">` + h.String() + `</header>`)
		}
	}
	if err := d.renderDOCXPart("word/document.xml", styles, b); err != nil {
		return err
	}
	for _, f := range numbered(zipList(d.files), "word/footer") {
		var h strings.Builder
		d.renderDOCXPart(f.Name, styles, &h)
		if strings.TrimSpace(stripTags(h.String())) != "" {
			b.WriteString(`<footer class="docfooter">` + h.String() + `</footer>`)
		}
	}
	return nil
}

func zipList(m map[string]*zip.File) []*zip.File {
	out := make([]*zip.File, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	return out
}

func (d *officeDoc) renderDOCXPart(part string, styles map[string]string, b *strings.Builder) error {
	dec, done, err := d.decoder(part)
	if err != nil {
		return err
	}
	defer done()
	rels := d.rels(part)
	s := &sinks{root: b}
	var run runFmt
	inPPr, inRPr, inTcPr, inT := false, false, false, false
	imgWidth := 0
	for b.Len() < maxText {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				s.push("p")
			case "pPr":
				inPPr = true
			case "pStyle":
				if p := s.top("p"); p != nil && inPPr {
					name := styles[attr(t, "val")]
					if name == "" {
						name = attr(t, "val")
					}
					if h := headingTag(name); h != "" {
						p.tag = h
					} else if strings.Contains(name, "list") {
						p.class = "li"
					}
				}
			case "numPr":
				if p := s.top("p"); p != nil && inPPr && p.tag == "" {
					p.class = "li"
				}
			case "jc":
				if p := s.top("p"); p != nil && inPPr {
					p.style = alignStyle(attr(t, "val"))
				}
			case "r":
				run = runFmt{}
			case "rPr":
				inRPr = true
			case "b":
				if inRPr {
					run.b = on(attr(t, "val"))
				}
			case "i":
				if inRPr {
					run.i = on(attr(t, "val"))
				}
			case "u":
				if inRPr {
					run.u = on(attr(t, "val"))
				}
			case "strike":
				if inRPr {
					run.s = on(attr(t, "val"))
				}
			case "t":
				inT = true
			case "tab":
				if !inPPr {
					s.out().WriteString(`<span class="tab"></span>`)
				}
			case "br", "cr":
				s.out().WriteString("<br>")
			case "extent":
				imgWidth = emuToPx(attr(t, "cx"))
			case "blip":
				if target := rels[attr(t, "embed")]; target != "" {
					s.out().WriteString(d.image(target, imgWidth))
				}
			case "tbl":
				s.push("tbl")
			case "tr":
				s.push("tr")
			case "tc":
				s.push("tc")
			case "tcPr":
				inTcPr = true
			case "gridSpan":
				if c := s.top("tc"); c != nil && inTcPr {
					c.colspan, _ = strconv.Atoi(attr(t, "val"))
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				if f := s.pop("p"); f != nil {
					s.out().WriteString(closeParagraph(f))
				}
			case "pPr":
				inPPr = false
			case "rPr":
				inRPr = false
			case "t":
				inT = false
			case "tcPr":
				inTcPr = false
			case "tbl":
				if f := s.pop("tbl"); f != nil {
					s.out().WriteString("<table>" + f.buf.String() + "</table>")
				}
			case "tr":
				if f := s.pop("tr"); f != nil {
					s.out().WriteString("<tr>" + f.buf.String() + "</tr>")
				}
			case "tc":
				if f := s.pop("tc"); f != nil {
					s.out().WriteString(cellOpen(f) + f.buf.String() + "</td>")
				}
			}
		case xml.CharData:
			if inT {
				s.out().WriteString(run.wrap(html.EscapeString(string(t))))
			}
		}
	}
	for len(s.stack) > 0 { // unbalanced file: flush what we have
		f := s.stack[len(s.stack)-1]
		s.stack = s.stack[:len(s.stack)-1]
		s.out().WriteString(f.buf.String())
	}
	return nil
}

// ---------- PowerPoint (.pptx) ----------

// relID returns the r:id attribute (the namespaced "id"), which links an
// element to another part of the file.
func relID(t xml.StartElement) string {
	for _, a := range t.Attr {
		if a.Name.Local == "id" && a.Name.Space != "" {
			return a.Value
		}
	}
	return ""
}

// slideOrder returns slide parts in presentation order.
func (d *officeDoc) slideOrder() []string {
	rels := d.rels("ppt/presentation.xml")
	var out []string
	if dec, done, err := d.decoder("ppt/presentation.xml"); err == nil {
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if t, ok := tok.(xml.StartElement); ok && t.Name.Local == "sldId" {
				if target := rels[relID(t)]; target != "" {
					out = append(out, target)
				}
			}
		}
		done()
	}
	if len(out) == 0 {
		for _, f := range numbered(zipList(d.files), "ppt/slides/slide") {
			out = append(out, f.Name)
		}
	}
	return out
}

func (d *officeDoc) renderPPTX(b *strings.Builder) error {
	slides := d.slideOrder()
	if len(slides) == 0 {
		return errors.New("no slides found")
	}
	for i, name := range slides {
		fmt.Fprintf(b, `<section class="slide"><div class="slideno">Slide %d</div>`, i+1)
		d.renderSlide(name, b)
		b.WriteString(`</section>`)
		if b.Len() > maxText {
			break
		}
	}
	return nil
}

func (d *officeDoc) renderSlide(part string, b *strings.Builder) {
	dec, done, err := d.decoder(part)
	if err != nil {
		return
	}
	defer done()
	rels := d.rels(part)
	s := &sinks{root: b}
	var run runFmt
	inT := false
	imgWidth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "sp":
				s.push("sp")
			case "ph":
				if sp := s.top("sp"); sp != nil {
					typ := attr(t, "type")
					sp.title = typ == "title" || typ == "ctrTitle"
				}
			case "p":
				p := s.push("p")
				if sp := s.top("sp"); sp != nil && sp.title {
					p.tag = "h2"
				}
			case "pPr":
				if p := s.top("p"); p != nil {
					p.style = alignStyle(map[string]string{"ctr": "center", "r": "right", "just": "justify"}[attr(t, "algn")])
				}
			case "r":
				run = runFmt{}
			case "rPr":
				run.b, run.i = attr(t, "b") == "1", attr(t, "i") == "1"
				run.u = attr(t, "u") != "" && attr(t, "u") != "none"
			case "t":
				inT = true
			case "br":
				s.out().WriteString("<br>")
			case "ext":
				imgWidth = emuToPx(attr(t, "cx"))
			case "blip":
				if target := rels[attr(t, "embed")]; target != "" {
					s.out().WriteString(d.image(target, imgWidth))
				}
			case "tbl":
				s.push("tbl")
			case "tr":
				s.push("tr")
			case "tc":
				c := s.push("tc")
				c.colspan, _ = strconv.Atoi(attr(t, "gridSpan"))
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "sp":
				if f := s.pop("sp"); f != nil {
					s.out().WriteString(f.buf.String())
				}
			case "p":
				if f := s.pop("p"); f != nil {
					s.out().WriteString(closeParagraph(f))
				}
			case "t":
				inT = false
			case "tbl":
				if f := s.pop("tbl"); f != nil {
					s.out().WriteString("<table>" + f.buf.String() + "</table>")
				}
			case "tr":
				if f := s.pop("tr"); f != nil {
					s.out().WriteString("<tr>" + f.buf.String() + "</tr>")
				}
			case "tc":
				if f := s.pop("tc"); f != nil {
					s.out().WriteString(cellOpen(f) + f.buf.String() + "</td>")
				}
			}
		case xml.CharData:
			if inT {
				s.out().WriteString(run.wrap(html.EscapeString(string(t))))
			}
		}
	}
}

// ---------- OpenDocument text and presentations (.odt, .odp) ----------

type odfStyle struct {
	fmt    runFmt
	align  string
	parent string
}

func (d *officeDoc) renderODF(b *strings.Builder, slides bool) error {
	dec, done, err := d.decoder("content.xml")
	if err != nil {
		return errors.New("not an OpenDocument file")
	}
	defer done()
	styles := map[string]*odfStyle{}
	var cur *odfStyle
	look := func(name string) odfStyle {
		var out odfStyle
		for i := 0; i < 5 && name != ""; i++ { // follow parents a few levels
			st := styles[name]
			if st == nil {
				break
			}
			out.fmt.b = out.fmt.b || st.fmt.b
			out.fmt.i = out.fmt.i || st.fmt.i
			out.fmt.u = out.fmt.u || st.fmt.u
			if out.align == "" {
				out.align = st.align
			}
			name = st.parent
		}
		return out
	}
	s := &sinks{root: b}
	inText := 0 // inside text:p / text:h
	slideNo := 0
	imgWidth := 0
	for b.Len() < maxText {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "style":
				cur = &odfStyle{parent: attr(t, "parent-style-name")}
				styles[attr(t, "name")] = cur
			case "text-properties":
				if cur != nil {
					cur.fmt.b = attr(t, "font-weight") == "bold"
					cur.fmt.i = attr(t, "font-style") == "italic"
					u := attr(t, "text-underline-style")
					cur.fmt.u = u != "" && u != "none"
				}
			case "paragraph-properties":
				if cur != nil {
					cur.align = attr(t, "text-align")
				}
			case "page":
				if slides {
					slideNo++
					s.push("page")
				}
			case "frame":
				fr := s.push("frame")
				fr.title = attr(t, "class") == "title"
				if w := attr(t, "width"); strings.HasSuffix(w, "cm") {
					v, _ := strconv.ParseFloat(strings.TrimSuffix(w, "cm"), 64)
					imgWidth = int(v * 37.8)
				}
			case "image":
				href := attr(t, "href")
				if href != "" && !strings.Contains(href, "://") {
					s.out().WriteString(d.image(strings.TrimPrefix(href, "./"), imgWidth))
				}
			case "h", "p":
				inText++
				p := s.push("p")
				st := look(attr(t, "style-name"))
				p.style = alignStyle(st.align)
				p.fmt = st.fmt
				if t.Name.Local == "h" {
					lvl, _ := strconv.Atoi(attr(t, "outline-level"))
					p.tag = "h" + strconv.Itoa(min(max(lvl+1, 2), 5))
				} else if fr := s.top("frame"); fr != nil && fr.title {
					p.tag = "h2"
				}
			case "span":
				sp := s.push("span")
				sp.fmt = look(attr(t, "style-name")).fmt
			case "list":
				s.push("list")
			case "list-item":
				s.push("li")
			case "table":
				s.push("tbl")
			case "table-row":
				s.push("tr")
			case "table-cell":
				c := s.push("tc")
				c.colspan, _ = strconv.Atoi(attr(t, "number-columns-spanned"))
			case "s":
				if inText > 0 {
					n, _ := strconv.Atoi(attr(t, "c"))
					s.out().WriteString(strings.Repeat("&nbsp;", max(1, min(n, 50))))
				}
			case "tab":
				if inText > 0 {
					s.out().WriteString(`<span class="tab"></span>`)
				}
			case "line-break":
				s.out().WriteString("<br>")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "style":
				cur = nil
			case "page":
				if f := s.pop("page"); f != nil {
					fmt.Fprintf(s.out(), `<section class="slide"><div class="slideno">Slide %d</div>%s</section>`, slideNo, f.buf.String())
				}
			case "frame":
				if f := s.pop("frame"); f != nil {
					s.out().WriteString(f.buf.String())
				}
			case "h", "p":
				inText--
				if f := s.pop("p"); f != nil {
					content := f.buf.String()
					f.buf.Reset()
					f.buf.WriteString(f.fmt.wrap(content))
					s.out().WriteString(closeParagraph(f))
				}
			case "span":
				if f := s.pop("span"); f != nil {
					s.out().WriteString(f.fmt.wrap(f.buf.String()))
				}
			case "list":
				if f := s.pop("list"); f != nil {
					s.out().WriteString("<ul>" + f.buf.String() + "</ul>")
				}
			case "list-item":
				if f := s.pop("li"); f != nil {
					s.out().WriteString("<li>" + f.buf.String() + "</li>")
				}
			case "table":
				if f := s.pop("tbl"); f != nil {
					s.out().WriteString("<table>" + f.buf.String() + "</table>")
				}
			case "table-row":
				if f := s.pop("tr"); f != nil {
					s.out().WriteString("<tr>" + f.buf.String() + "</tr>")
				}
			case "table-cell":
				if f := s.pop("tc"); f != nil {
					s.out().WriteString(cellOpen(f) + f.buf.String() + "</td>")
				}
			}
		case xml.CharData:
			if inText > 0 {
				s.out().WriteString(html.EscapeString(string(t)))
			}
		}
	}
	return nil
}

// ---------- spreadsheets ----------

type gridCell struct {
	text string
	num  bool
}

type sheet struct {
	name string
	rows [][]gridCell
}

const (
	maxRows = 2000
	maxCols = 60
)

func renderSheets(sheets []sheet, b *strings.Builder) {
	for _, sh := range sheets {
		// Drop empty rows at the end and empty columns on the right.
		last := -1
		cols := 0
		for i, r := range sh.rows {
			for j, c := range r {
				if strings.TrimSpace(c.text) != "" {
					last = i
					cols = max(cols, j+1)
				}
			}
		}
		if last < 0 {
			continue
		}
		fmt.Fprintf(b, `<h2 class="sheetname">%s</h2><div class="gridwrap"><table class="grid">`, html.EscapeString(sh.name))
		for i := 0; i <= last && i < maxRows; i++ {
			b.WriteString("<tr>")
			r := sh.rows[i]
			for j := 0; j < cols && j < maxCols; j++ {
				var c gridCell
				if j < len(r) {
					c = r[j]
				}
				if c.num {
					b.WriteString(`<td class="num">`)
				} else {
					b.WriteString("<td>")
				}
				b.WriteString(strings.ReplaceAll(html.EscapeString(c.text), "\n", "<br>"))
				b.WriteString("</td>")
			}
			b.WriteString("</tr>")
		}
		b.WriteString("</table></div>")
		if last >= maxRows || cols > maxCols {
			fmt.Fprintf(b, `<p class="note">Showing the first %d rows and %d columns. Download the file to see everything.</p>`, min(last+1, maxRows), min(cols, maxCols))
		}
	}
}

// colIndex turns a cell reference like "AB12" into a 0-based column.
func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
	}
	return n - 1
}

func (d *officeDoc) renderXLSX(b *strings.Builder) error {
	shared := xlsxShared(d.files["xl/sharedStrings.xml"])
	dates := xlsxDateStyles(d.files["xl/styles.xml"])
	rels := d.rels("xl/workbook.xml")
	type ref struct{ name, part string }
	var order []ref
	if dec, done, err := d.decoder("xl/workbook.xml"); err == nil {
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if t, ok := tok.(xml.StartElement); ok && t.Name.Local == "sheet" {
				if part := rels[relID(t)]; part != "" {
					order = append(order, ref{attr(t, "name"), part})
				}
			}
		}
		done()
	}
	if len(order) == 0 {
		for i, f := range numbered(zipList(d.files), "xl/worksheets/sheet") {
			order = append(order, ref{fmt.Sprintf("Sheet %d", i+1), f.Name})
		}
	}
	var sheets []sheet
	for _, o := range order {
		sh := sheet{name: o.name}
		dec, done, err := d.decoder(o.part)
		if err != nil {
			continue
		}
		var row []gridCell
		rowIdx := -1
		var cellType, cellRef string
		var cellStyle int
		var val strings.Builder
		inV := false
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "row":
					r, _ := strconv.Atoi(attr(t, "r"))
					if r > 0 {
						rowIdx = r - 1
					} else {
						rowIdx++
					}
					row = nil
				case "c":
					cellType, cellRef = attr(t, "t"), attr(t, "r")
					cellStyle, _ = strconv.Atoi(attr(t, "s"))
					val.Reset()
				case "v", "t":
					inV = true
				}
			case xml.EndElement:
				switch t.Name.Local {
				case "v", "t":
					inV = false
				case "c":
					v := val.String()
					c := gridCell{text: v}
					switch {
					case cellType == "s":
						if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(shared) {
							c.text = shared[i]
						}
					case cellType == "b":
						c.text = map[string]string{"1": "TRUE", "0": "FALSE"}[v]
					case cellType == "" || cellType == "n":
						if dates[cellStyle] {
							c.text = excelDate(v)
						} else {
							c.num = v != ""
						}
					}
					col := len(row)
					if cellRef != "" {
						col = colIndex(cellRef)
					}
					if col >= 0 && col < maxCols*2 {
						for len(row) <= col {
							row = append(row, gridCell{})
						}
						row[col] = c
					}
				case "row":
					if rowIdx >= 0 && rowIdx < maxRows*2 {
						for len(sh.rows) <= rowIdx {
							sh.rows = append(sh.rows, nil)
						}
						sh.rows[rowIdx] = row
					}
				}
			case xml.CharData:
				if inV {
					val.Write(t)
				}
			}
		}
		done()
		sheets = append(sheets, sh)
	}
	renderSheets(sheets, b)
	return nil
}

func (d *officeDoc) renderODS(b *strings.Builder) error {
	dec, done, err := d.decoder("content.xml")
	if err != nil {
		return errors.New("not an OpenDocument file")
	}
	defer done()
	var sheets []sheet
	var sh *sheet
	var row []gridCell
	var cell gridCell
	rowRepeat, cellRepeat := 1, 1
	inText := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "table":
				sheets = append(sheets, sheet{name: attr(t, "name")})
				sh = &sheets[len(sheets)-1]
			case "table-row":
				row = nil
				rowRepeat, _ = strconv.Atoi(attr(t, "number-rows-repeated"))
			case "table-cell", "covered-table-cell":
				cell = gridCell{}
				switch attr(t, "value-type") {
				case "float", "currency", "percentage":
					cell.num = true
				}
				cellRepeat, _ = strconv.Atoi(attr(t, "number-columns-repeated"))
			case "p":
				if inText == 0 && cell.text != "" {
					cell.text += "\n"
				}
				inText++
			case "s":
				if inText > 0 {
					cell.text += " "
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				inText--
			case "table-cell", "covered-table-cell":
				// Empty cells are often repeated thousands of times.
				n := max(1, cellRepeat)
				if cell.text == "" {
					n = min(n, maxCols)
				}
				for i := 0; i < n && len(row) < maxCols*2; i++ {
					row = append(row, cell)
				}
			case "table-row":
				if sh == nil {
					break
				}
				n := max(1, rowRepeat)
				empty := true
				for _, c := range row {
					if c.text != "" {
						empty = false
					}
				}
				if empty {
					n = min(n, 1)
				}
				for i := 0; i < n && len(sh.rows) < maxRows*2; i++ {
					sh.rows = append(sh.rows, row)
				}
			}
		case xml.CharData:
			if inText > 0 {
				cell.text += string(t)
			}
		}
	}
	renderSheets(sheets, b)
	return nil
}
