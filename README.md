# paperfull

A document archive for your own machine. You throw scans, PDFs and office
files at it, it reads them, files them, and lets you find them again.

I liked the idea of paperless-ngx but not running Docker, Redis, Celery,
Postgres and a pile of Python workers to keep a few hundred letters in
order. paperfull is one Go binary and one SQLite file. It idles at around
20 MB of RAM.

## What it does

- Reads scans and photos with tesseract and stores a searchable PDF next to
  the original. PDFs that already have text are left alone. Pages that went
  through the scanner sideways or upside down get turned the right way.
- Guesses sender, type, tags and date for each new document. It learns from
  the documents you've already sorted, and you can add your own rules
  ("anything mentioning 'Stadtwerke' is from Stadtwerke").
- Optionally asks a local language model (Ollama) who sent a letter and what
  it is about. Useful for senders it hasn't seen before. Nothing leaves your
  machine.
- Search that finds words inside words: `rechnung` matches
  "Stromrechnung". Filters like `from:stadtwerke tag:steuer after:2023`.
- Keeps the files on disk in plain folders, `Sender/Year/Date Title.pdf`,
  renamed when you change something. You can browse your archive without
  paperfull running, and nothing is locked into a database.
- Watches a folder for new scans, and has a REST API for scripts.

New documents land in an inbox. You open one, fix whatever it got wrong,
press Done and get the next one. Every document you sort makes the guesses
for the next ones better.

## Requirements

- Go 1.23 or newer to build it
- tesseract, with the language data you need (and `osd` for rotated pages)
- poppler (`pdftotext`, `pdftoppm`, `pdfinfo`, `pdfunite`)

Optional:

- LibreOffice, for real PDF previews of Word/Excel/PowerPoint files
- Ollama, for AI suggestions

```sh
# Arch
pacman -S tesseract tesseract-data-deu tesseract-data-eng tesseract-data-osd poppler

# Debian / Ubuntu
apt install tesseract-ocr tesseract-ocr-deu tesseract-ocr-osd poppler-utils

# macOS
brew install tesseract tesseract-lang poppler
```

## Running it

```sh
go build -o paperfull ./cmd/paperfull
./paperfull -data ~/.local/share/paperfull -consume ~/Scans/inbox
```

Then open http://127.0.0.1:8484. Drop a file onto the page or into
`~/Scans/inbox` and it shows up in the inbox a few seconds later.
`samples/` has a scanned invoice and a Word letter to try it with.

By default it only listens on localhost and has no login. If you want to
reach it from other machines, set a password:

```sh
./paperfull -listen 0.0.0.0:8484 -token 'something long'
```

`contrib/paperfull.service` is a systemd user unit if you want it running
in the background.

## Options

Every option can also be set as an environment variable. `paperfull -h`
lists all of them; these are the ones you're likely to touch:

| Option | Variable | Default |
|---|---|---|
| `-data` | `PAPERFULL_DATA` | `./data` |
| `-consume` | `PAPERFULL_CONSUME` | off |
| `-listen` | `PAPERFULL_LISTEN` | `127.0.0.1:8484` |
| `-token` | `PAPERFULL_TOKEN` | no login |
| `-lang` | `PAPERFULL_OCR_LANG` | `deu+eng` |
| `-date-order` | `PAPERFULL_DATE_ORDER` | `DMY` (so 03/04 is 3 April) |
| `-filename-format` | `PAPERFULL_FILENAME_FORMAT` | `{correspondent}/{year}/{created} {title}` |
| `-ai` | `PAPERFULL_AI_URL` | off |
| `-ai-model` | `PAPERFULL_AI_MODEL` | `qwen2.5:7b` |
| `-libreoffice` | `PAPERFULL_LIBREOFFICE` | `auto` |

The filename format understands `{correspondent}`, `{type}`, `{title}`,
`{created}`, `{year}`, `{month}`, `{day}`, `{id}`, `{tags}` and
`{original_name}`.

## File types

| Files | What happens |
|---|---|
| PDF | Uses the text if there is any, otherwise OCR and a searchable copy |
| JPEG, PNG, TIFF (also multi-page), WebP, GIF, BMP | OCR, stored with a searchable PDF |
| .docx, .xlsx, .pptx, .odt, .ods, .odp | Text is read straight from the file. With LibreOffice installed you also get a PDF that looks like the original; without it, a simpler preview with headings, tables and images |
| .txt | Stored as it is |

Old `.doc`/`.xls` files and iPhone HEIC photos aren't supported yet.

LibreOffice is picked up automatically if it's installed, including the
macOS app. Use `-libreoffice off` to skip it.

## The watched folder

paperfull waits until a file stops changing, so a scanner that's still
writing doesn't get interrupted. Once a file is stored it's removed from the
folder. Files that can't be added (broken, unsupported, already in the
archive) go to `.failed/` with a short note explaining why.

Sub-folder names become tags. Scanning into `inbox/Steuer/` tags the
document "Steuer". Turn that off with `-consume-subdir-tags=false`.

## Password-protected PDFs

Bank statements and payslips often need a password to open. paperfull
notices, keeps the file aside and asks you for the password, either right
away in a dialog or later from the Activity page. Once unlocked, the
document is stored without the password, so you're never asked again.
PDFs that only block printing or editing are added without asking.

Locked files wait in `data/locked/` and survive restarts until you unlock
or discard them.

## How sorting works

Each sender, type and tag has a setting for when it's assigned on its own:

- **Learn from my documents** (default). New documents get the labels of
  the most similar documents you've already sorted. Your own name and
  address are ignored, since they're on every letter.
- **Text contains any / all of these words**, **this exact phrase**, or a
  **regular expression**.
- **Never**, if you only want to set it by hand.

Rules always win. Learned guesses are only trusted when several similar
documents agree.

## AI suggestions

If you run [Ollama](https://ollama.com), paperfull can ask a model to read
each new document:

```sh
ollama pull qwen2.5:7b
./paperfull -ai http://127.0.0.1:11434
```

It suggests sender, type, title and date. A sender you already have is
filled in directly, even if the letter spells it differently ("Stadtwerke
Musterstadt GmbH" and "Stadtwerke Musterstadt" are the same). A new sender
is only pre-filled and gets created when you press Done, so your list
doesn't fill up with junk. Scanner file names like `scan_0042.pdf` get a
proper title.

With a 7B model on a mid-range GPU this takes about a second per document.
If Ollama isn't running, documents are processed as usual without
suggestions. Older documents have a "Fill in with AI" button.

## Search

| Query | Finds |
|---|---|
| `strom rechnung` | documents with both words, also inside longer words |
| `"exact phrase"` | that phrase |
| `gas OR strom` | either word |
| `-mahnung` | everything without that word |
| `title:vertrag` | only in titles |
| `tag:steuer`, `-tag:paid`, `tag:none` | by tag |
| `from:stadtwerke`, `from:none` | by sender |
| `type:invoice` | by type |
| `after:2023-05`, `before:2024`, `year:2022` | by document date |
| `inbox:yes` | not reviewed yet |

Names with spaces need quotes: `tag:"Steuer 2023"`. You rarely need to type
any of this; the filter buttons above the results do it for you.

## API

Send `Authorization: Bearer <token>` if you've set a token.

```sh
# upload, optionally with metadata (ids or names; new names are created)
curl -H "Authorization: Bearer $TOKEN" \
     -F document=@scan.pdf -F tags=Steuer,2024 -F correspondent=Finanzamt \
     http://127.0.0.1:8484/api/documents

# same, as raw body, waiting until it's processed
curl -H "Authorization: Bearer $TOKEN" --data-binary @scan.pdf \
     "http://127.0.0.1:8484/api/documents?filename=scan.pdf&wait=true"

# search
curl -H "Authorization: Bearer $TOKEN" \
     "http://127.0.0.1:8484/api/documents?q=from:stadtwerke+year:2025"
```

| Endpoint | |
|---|---|
| `GET /api/documents?q=&sort=&limit=&offset=` | search |
| `POST /api/documents` | upload, returns a task |
| `GET`, `PATCH`, `DELETE /api/documents/{id}` | read, change, delete |
| `POST /api/documents/bulk` | change or delete several at once |
| `GET /api/documents/{id}/original`, `/archive`, `/thumb` | files |
| `POST /api/documents/{id}/reprocess` | read the document again |
| `POST /api/documents/{id}/ai` | ask the AI, without saving |
| `GET /api/tasks`, `/api/tasks/{id}` | processing status |
| `POST /api/tasks/{id}/unlock` | `{"password": "…"}` for a locked PDF |
| `DELETE /api/tasks/{id}` | discard a locked PDF |
| `GET /api/events` | live updates (server-sent events) |
| `GET`, `POST /api/tags`, `/api/correspondents`, `/api/document_types` | list, create |
| `GET`, `PATCH`, `DELETE /api/{kind}/{id}` | one tag, sender or type |

`PATCH` takes `title`, `created` (YYYY-MM-DD), `notes`, `inbox`,
`correspondent` and `document_type` (id, name or `null`), and `tags`,
`add_tags`, `remove_tags`.

## Where your data is

```
data/
  paperfull.db      sorting, text and search index
  originals/        your files, as uploaded
  archive/          searchable PDFs paperfull made
  locked/           password-protected PDFs waiting for their password
  thumbs/
```

To back up, copy the whole folder. If you ever only restore the database,
run `paperfull reindex` to rebuild the search index.

## Things it doesn't do (yet)

- Multiple users. There's one login.
- Email import.

## Development

```sh
go test ./...
```

The tests don't need Ollama: they use a fake model server. Tests that
need tesseract, poppler or LibreOffice skip themselves when those aren't
installed.
