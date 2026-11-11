package web

import (
	"html"
	"net/http"
	"path/filepath"

	"paperfull/internal/ocr"
)

// officePage wraps a rendered office document in a page that looks like paper.
const officePage = `<!doctype html><html><head><meta charset="utf-8"><meta name="color-scheme" content="light dark">
<style>
*{box-sizing:border-box}
html{background:#e7e7e2}
@media (prefers-color-scheme:dark){html{background:#2e2e2c}.note{color:#a8a8a1}.paper,.slide{box-shadow:0 1px 3px #0006,0 8px 28px #0004}}
body{margin:0;padding:24px 16px 48px;font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Arial,sans-serif;color:#1c1c1a}
.note{max-width:820px;margin:0 auto 12px;font-size:12.5px;color:#6b6b66}
.paper{max-width:820px;margin:0 auto;background:#fff;padding:56px 64px;box-shadow:0 1px 3px #0002,0 8px 28px #0001;border-radius:3px;overflow-wrap:anywhere}
.paper.wide{max-width:none;padding:28px 32px}
h1,h2,h3,h4,h5{line-height:1.25;margin:1.1em 0 .45em}
h1{font-size:1.7em}h2{font-size:1.35em}h3{font-size:1.15em}h4,h5{font-size:1em}
p{margin:0 0 .55em}p.blank{min-height:.8em}p.li{padding-left:1.3em;position:relative}p.li::before{content:"•";position:absolute;left:.3em}
.tab{display:inline-block;width:3em}
img{max-width:100%;height:auto;vertical-align:middle}
table{border-collapse:collapse;margin:.6em 0 1em;max-width:100%}
td{border:1px solid #d4d4ce;padding:.35em .6em;vertical-align:top}
td p{margin:0}
ul{margin:.2em 0 .7em;padding-left:1.4em}li p{margin:0}
.docheader{color:#555;border-bottom:1px solid #e5e5e0;margin-bottom:1.5em;padding-bottom:.6em}
.docfooter{color:#777;font-size:.85em;border-top:1px solid #e5e5e0;margin-top:2em;padding-top:.6em}
.slide{background:#fff;max-width:820px;margin:0 auto 20px;aspect-ratio:16/9;padding:36px 48px;box-shadow:0 1px 3px #0002,0 8px 28px #0001;border-radius:4px;position:relative;overflow:hidden}
.slide h2{margin-top:0}
.slideno{position:absolute;right:14px;bottom:10px;font-size:12px;color:#999}
.sheetname{font-size:1em;margin:1.4em 0 .5em;color:#555}.sheetname:first-child{margin-top:0}
.gridwrap{overflow:auto}
.grid{font-size:13px;margin:0}.grid td{white-space:nowrap;padding:.25em .55em}.grid td.num{text-align:right;font-variant-numeric:tabular-nums}
.grid tr:first-child td{background:#f3f3ef;font-weight:600}
pre{white-space:pre-wrap;font:13px/1.6 ui-monospace,Menlo,Consolas,monospace}
</style></head><body>`

func (s *Server) uiOfficeView(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	d, err := s.Store.GetDocument(r.Context(), id)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	// Only our own generated markup and inlined images: no scripts, no requests.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; frame-ancestors 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(officePage))

	body, err := "", ocr.ErrUnsupported
	if ocr.IsOffice(d.Mime) {
		body, err = ocr.RenderOfficeHTML(filepath.Join(s.Ingester.OriginalsDir(), d.Path), d.Mime)
	}
	if err != nil || body == "" {
		if err != nil {
			s.Log.Warn("office preview", "id", id, "err", err)
		}
		w.Write([]byte(`<p class="note">This file can't be shown with formatting, here is its text.</p><div class="paper"><pre>` + html.EscapeString(d.Content) + `</pre></div></body></html>`))
		return
	}
	wide := ""
	if d.Mime == ocr.MimeXLSX || d.Mime == ocr.MimeODS {
		wide = " wide"
	}
	note := `<p class="note">Simplified preview of the ` + ocr.Label(d.Mime) + ` file. Download it to see the exact layout.</p>`
	if d.Mime == ocr.MimePPTX || d.Mime == ocr.MimeODP {
		w.Write([]byte(note + body + `</body></html>`))
		return
	}
	w.Write([]byte(note + `<div class="paper` + wide + `">` + body + `</div></body></html>`))
}
