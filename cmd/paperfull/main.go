// paperfull: a small, self-hosted document archive with OCR, auto-sorting and full-text search.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"paperfull/internal/classify"
	"paperfull/internal/consume"
	"paperfull/internal/ingest"
	"paperfull/internal/llm"
	"paperfull/internal/ocr"
	"paperfull/internal/store"
	"paperfull/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

const usage = `paperfull - document archive with OCR, auto-sorting and full-text search

Usage:
  paperfull [flags]            run the server (default)
  paperfull reindex [flags]    rebuild the search index
  paperfull retrain [flags]    train the classifier once and print stats

Every flag can also be set with an environment variable (shown in brackets).

Flags:
`

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("paperfull", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}
	dataDir := fs.String("data", env("PAPERFULL_DATA", "./data"), "data directory: database, originals, archive [PAPERFULL_DATA]")
	consumeDir := fs.String("consume", env("PAPERFULL_CONSUME", ""), "folder to watch for new documents, empty = off [PAPERFULL_CONSUME]")
	listen := fs.String("listen", env("PAPERFULL_LISTEN", "127.0.0.1:8484"), "HTTP listen address [PAPERFULL_LISTEN]")
	token := fs.String("token", env("PAPERFULL_TOKEN", ""), "access token for API and web UI, empty = no auth [PAPERFULL_TOKEN]")
	lang := fs.String("lang", env("PAPERFULL_OCR_LANG", "deu+eng"), "tesseract languages [PAPERFULL_OCR_LANG]")
	dpi := fs.Int("dpi", envInt("PAPERFULL_OCR_DPI", 300), "resolution for OCR of scanned PDFs [PAPERFULL_OCR_DPI]")
	ocrMode := fs.String("ocr", env("PAPERFULL_OCR_MODE", "auto"), "auto = skip PDFs that already have text, always = OCR everything [PAPERFULL_OCR_MODE]")
	psm := fs.Int("psm", envInt("PAPERFULL_OCR_PSM", 0), "tesseract page segmentation mode, 0 = pick automatically [PAPERFULL_OCR_PSM]")
	jobs := fs.Int("ocr-jobs", envInt("PAPERFULL_OCR_JOBS", max(1, runtime.NumCPU()-1)), "pages OCR'd in parallel [PAPERFULL_OCR_JOBS]")
	workers := fs.Int("workers", envInt("PAPERFULL_WORKERS", 1), "documents processed in parallel [PAPERFULL_WORKERS]")
	format := fs.String("filename-format", env("PAPERFULL_FILENAME_FORMAT", ingest.DefaultFilenameFormat),
		"where files are stored inside the data dir; placeholders: {correspondent} {type} {title} {created} {year} {month} {day} {id} {tags} {original_name} [PAPERFULL_FILENAME_FORMAT]")
	dateOrder := fs.String("date-order", env("PAPERFULL_DATE_ORDER", "DMY"), "how to read ambiguous dates like 03/04/2024: DMY or MDY [PAPERFULL_DATE_ORDER]")
	subdirTags := fs.Bool("consume-subdir-tags", envBool("PAPERFULL_CONSUME_SUBDIR_TAGS", true), "tag documents with the names of the sub-folders they were dropped into [PAPERFULL_CONSUME_SUBDIR_TAGS]")
	llmURL := fs.String("ai", env("PAPERFULL_AI_URL", ""), "Ollama address for AI suggestions of sender, type, title and date, e.g. http://127.0.0.1:11434; empty = off [PAPERFULL_AI_URL]")
	llmModel := fs.String("ai-model", env("PAPERFULL_AI_MODEL", "qwen2.5:7b"), "Ollama model to use [PAPERFULL_AI_MODEL]")
	loFlag := fs.String("libreoffice", env("PAPERFULL_LIBREOFFICE", "auto"), "LibreOffice for real PDF previews of office files: auto (use it if installed), off, or the path to soffice [PAPERFULL_LIBREOFFICE]")
	debug := fs.Bool("debug", envBool("PAPERFULL_DEBUG", false), "verbose logging [PAPERFULL_DEBUG]")
	fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fatal(log, "creating data dir", err)
	}
	st, err := store.Open(filepath.Join(*dataDir, "paperfull.db"))
	if err != nil {
		fatal(log, "opening database", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cls := classify.New(st, log)
	switch cmd {
	case "reindex":
		if err := st.Reindex(ctx); err != nil {
			fatal(log, "reindex", err)
		}
		log.Info("search index rebuilt")
		return
	case "retrain":
		if err := cls.Train(ctx); err != nil {
			fatal(log, "retrain", err)
		}
		return
	case "serve":
	default:
		fs.Usage()
		os.Exit(2)
	}

	if err := ocr.CheckTools(); err != nil {
		fatal(log, "checking dependencies", err)
	}
	langs := ocr.Languages()
	for _, l := range strings.Split(*lang, "+") {
		if !slices.Contains(langs, l) {
			fatal(log, "checking OCR languages", fmt.Errorf("tesseract language %q is not installed (have: %s)", l, strings.Join(langs, ", ")))
		}
	}
	hasOSD := slices.Contains(langs, "osd")
	if *psm == 0 {
		*psm = 3
		if hasOSD {
			*psm = 1 // also copes with rotated text
		}
	}
	if !hasOSD {
		log.Warn("tesseract 'osd' data not installed: rotated pages won't be turned upright")
	}
	if *ocrMode != "auto" && *ocrMode != "always" {
		fatal(log, "flags", errors.New("-ocr must be auto or always"))
	}

	var ai *llm.Client
	if *llmURL != "" {
		ai = llm.New(*llmURL, *llmModel)
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := ai.Check(cctx); err != nil {
			log.Warn("AI suggestions may not work", "err", err)
		} else {
			log.Info("AI suggestions on", "model", *llmModel, "url", *llmURL)
		}
		cancel()
	}

	lo := ""
	switch *loFlag {
	case "off", "no", "false", "":
	case "auto":
		lo = ocr.FindLibreOffice()
	default:
		lo = *loFlag
	}
	if lo != "" {
		log.Info("office files get PDF previews", "libreoffice", lo)
	} else {
		log.Info("LibreOffice not used: office files get a simplified preview")
	}

	in, err := ingest.New(ingest.Config{
		LLM:            ai,
		DataDir:        *dataDir,
		FilenameFormat: *format,
		Workers:        *workers,
		DayFirst:       !strings.EqualFold(*dateOrder, "MDY"),
		OCR: ocr.Config{Languages: *lang, DPI: *dpi, Jobs: *jobs, Mode: *ocrMode, PSM: *psm, Rotate: hasOSD,
			LibreOffice: lo, LibreOfficeProfile: absPath(filepath.Join(*dataDir, "libreoffice-profile"))},
	}, st, cls, log)
	if err != nil {
		fatal(log, "setting up storage", err)
	}

	go cls.Run(ctx, 10*time.Second)
	go in.Run(ctx)
	if *consumeDir != "" {
		w := &consume.Watcher{Dir: *consumeDir, SubdirTags: *subdirTags, Ingester: in, Store: st, Log: log}
		go func() {
			if err := w.Run(ctx); err != nil {
				log.Error("consume folder watcher stopped", "err", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&web.Server{Store: st, Ingester: in, Cls: cls, Token: *token, Log: log, ConsumeDir: *consumeDir}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Request contexts end on shutdown, which closes live event streams.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if *token == "" && !strings.HasPrefix(*listen, "127.") && !strings.HasPrefix(*listen, "localhost") {
		log.Warn("no -token set and listening on a non-local address: anyone on the network can read your documents")
	}
	log.Info("paperfull running", "url", "http://"+*listen, "data", *dataDir, "ocr", *lang, "psm", *psm)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(log, "http server", err)
	}
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	os.Exit(1)
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
