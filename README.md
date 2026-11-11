# paperfull

A small, self-hosted document archive in the spirit of paperless-ngx: one Go
binary and one SQLite file. No Docker, Redis, Celery or Postgres.

- **OCR** with tesseract. PDFs that already contain text are not touched;
  scans and photos get a searchable PDF. Sideways and upside-down pages are
  turned upright.
- **Auto-sorting.** Correspondent, document type and tags are set by your
  rules (any / all / literal / regex) or learned from documents you have
  already reviewed. The date is read from the document text.
- **Search** that finds words inside words ("rechnung" → "Stromrechnung"),
  ignores accents, and has filters such as `tag:steuer from:stadtwerke after:2023`.
- **Tidy files on disk.** Originals are stored as
  `Correspondent/Year/Date Title.pdf` and renamed when you change metadata.
  You can browse the archive without paperfull.
- **Folder watching and a REST API** for getting documents in.

## Requirements

`tesseract` with your language data (plus `osd` for rotation), and `poppler`
(`pdftotext`, `pdftoppm`, `pdfinfo`, `pdfunite`).

```sh
# Arch
pacman -S tesseract tesseract-data-deu tesseract-data-eng tesseract-data-osd poppler
# Debian/Ubuntu
apt install tesseract-ocr tesseract-ocr-deu tesseract-ocr-osd poppler-utils
```

## Build and run

```sh
go build -o paperfull ./cmd/paperfull
./paperfull -data ~/.local/share/paperfull -consume ~/Scans/inbox
# open http://127.0.0.1:8484
```

Every flag has an environment variable (`./paperfull -h`). The ones you'll
most likely want:

| flag | env | default |
|---|---|---|
| `-data` | `PAPERFULL_DATA` | `./data` |
| `-consume` | `PAPERFULL_CONSUME` | off |
| `-listen` | `PAPERFULL_LISTEN` | `127.0.0.1:8484` |
| `-token` | `PAPERFULL_TOKEN` | none (no login) |
| `-lang` | `PAPERFULL_OCR_LANG` | `deu+eng` |
| `-filename-format` | `PAPERFULL_FILENAME_FORMAT` | `{correspondent}/{year}/{created} {title}` |
| `-date-order` | `PAPERFULL_DATE_ORDER` | `DMY` |
| `-ai` | `PAPERFULL_AI_URL` | off, e.g. `http://127.0.0.1:11434` |
| `-ai-model` | `PAPERFULL_AI_MODEL` | `qwen2.5:7b` |
| `-libreoffice` | `PAPERFULL_LIBREOFFICE` | `auto` (use if installed), `off`, or path to `soffice` |

`contrib/paperfull.service` is a systemd user unit.

## Supported files

| Files | How |
|---|---|
| PDF | Text layer used when present, otherwise OCR plus a searchable copy |
| JPEG, PNG, TIFF (multi-page), WebP, GIF, BMP | OCR, stored with a searchable PDF |
| Word, Excel, PowerPoint (.docx, .xlsx, .pptx) and LibreOffice (.odt, .ods, .odp) | Text read directly from the file. If LibreOffice is installed, also turned into a PDF with the original layout (preview, thumbnail, printing); otherwise shown as a simplified formatted preview |
| Plain text | As is |

LibreOffice is optional and found automatically, also inside the macOS app
(`-libreoffice off` to skip it, or give the path to `soffice`). Converting
takes a second or two per file. Old binary formats (.doc, .xls) and HEIC
photos aren't supported. To get PDFs for office files added earlier, select
them and choose **Read text again**.

## Getting documents in

**Consume folder.** Files dropped into the folder are picked up once they
stop changing, then deleted after they were stored. Files that fail
(unsupported, broken, duplicate) are moved to `.failed/` next to a `.err`
note. Sub-folder names become tags, so scanning into `inbox/Steuer/2024/`
tags the document `Steuer` and `2024`. Switch that off with
`-consume-subdir-tags=false`.

**API.**

```sh
# multipart, with optional metadata (ids or names; unknown names are created)
curl -H "Authorization: Bearer $TOKEN" \
     -F document=@scan.pdf -F tags=Steuer,2024 -F correspondent=Finanzamt \
     http://127.0.0.1:8484/api/documents

# raw body, wait until processed
curl -H "Authorization: Bearer $TOKEN" --data-binary @scan.pdf \
     "http://127.0.0.1:8484/api/documents?filename=scan.pdf&wait=true"
```

**Web UI.** Drag and drop files anywhere on the page.

## Sorting

New documents land in the **inbox** with whatever the classifier suggested.
Open one, fix what's wrong and press **Save & done** to go to the next one.
Every reviewed document teaches the classifier.

Each tag, correspondent and document type has a matching mode:

- `auto` (default): learned. A new document gets the labels of the most
  similar reviewed documents, so one example is enough to start. Tags need
  two examples. Once the sender is known, type and tags are taken from that
  sender's earlier documents.
- `any` / `all`: whole words from the match text (use `"quotes"` for phrases).
- `literal`: exact text. `regex`: a regular expression.
- `none`: never assigned automatically.

Rules win over learning for correspondent and type. Tags from both are merged.

## AI suggestions (optional)

With a local [Ollama](https://ollama.com), paperfull reads each new document
and suggests sender, type, title and date. It's off by default and nothing
leaves your machine.

```sh
ollama pull qwen2.5:7b
./paperfull -ai http://127.0.0.1:11434            # -ai-model to pick another model
```

- Your rules and what paperfull learned from reviewed documents still win.
  The model fills the gaps, which mostly means senders it has never seen.
- A suggested sender that already exists is matched loosely
  ("Stadtwerke Musterstadt GmbH" → "Stadtwerke Musterstadt").
- A new sender or type is only pre-filled, marked "new · suggested", and
  created when you save the document. Nothing gets created behind your back.
- Junk file names like `scan_0042.pdf` get a real title.
- On any document page, **Fill in with AI** asks again for documents that
  were added before.
- If Ollama is down, documents are processed as usual without suggestions.

A 7B model on a mid-range GPU takes about a second per document.

## Search syntax

| query | meaning |
|---|---|
| `strom rechnung` | all words, also inside longer words |
| `"exact phrase"` | phrase |
| `gas OR strom`, `-mahnung` | either / exclude |
| `title:vertrag` | title only |
| `tag:steuer`, `-tag:paid`, `tag:none` | tags |
| `from:stadtwerke`, `correspondent:none` | correspondent |
| `type:invoice` | document type |
| `after:2023-05`, `before:2024`, `year:2022` | document date (`after` is inclusive) |
| `inbox:yes` | not yet reviewed |
| `id:42` | one document |

Names with spaces need quotes: `tag:"Steuer 2023"`.

## API overview

All endpoints take `Authorization: Bearer <token>` when a token is set.

| | |
|---|---|
| `GET /api/documents?q=&sort=&limit=&offset=` | search (sort: `created`, `-created`, `added`, `-added`, `title`) |
| `POST /api/documents` | upload (see above); returns a task |
| `GET/PATCH/DELETE /api/documents/{id}` | read, change, delete |
| `POST /api/documents/bulk` | `{"documents":[1,2], "add_tags":["x"], "correspondent":"y", "inbox":false}` or `{"documents":[…], "delete":true}` |
| `GET /api/documents/{id}/original` · `/archive` · `/thumb` | files |
| `GET /api/documents/{id}/suggestions` | what the classifier would assign |
| `POST /api/documents/{id}/reprocess` | redo OCR |
| `GET /api/tasks`, `GET /api/tasks/{id}` | processing status |
| `GET/POST /api/tags` · `/api/correspondents` · `/api/document_types` | list, create |
| `GET/PATCH/DELETE /api/{kind}/{id}` | read, change, delete one |
| `GET /api/stats`, `POST /api/retrain` | |

PATCH fields: `title`, `created` (YYYY-MM-DD), `notes`, `inbox`,
`correspondent` and `document_type` (id, name or `null`), `tags` (replaces
all), `add_tags`, `remove_tags`.

## Data layout

```
data/
  paperfull.db      metadata, text, search index
  originals/        your files, in the filename format
  archive/          searchable PDFs made by OCR, same layout
  thumbs/
```

Back up the whole directory. After restoring only the database, run
`paperfull reindex` to rebuild the search index.
