// Package llm asks a local language model (via Ollama) to read a document
// and suggest its sender, type, title and date.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type Client struct {
	URL   string // e.g. http://127.0.0.1:11434
	Model string // e.g. qwen2.5:7b
	HTTP  *http.Client
}

func New(url, model string) *Client {
	return &Client{
		URL:   strings.TrimRight(url, "/"),
		Model: model,
		// Generous: the first request has to load the model into memory.
		HTTP: &http.Client{Timeout: 3 * time.Minute},
	}
}

// Suggestion is what the model thinks about a document. Empty fields mean
// "don't know".
type Suggestion struct {
	Sender string `json:"sender"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Date   string `json:"date"`
}

const systemPrompt = `You read the OCR text of a scanned document from someone's personal archive and extract metadata. Answer with JSON only.
sender: who issued or sent the document (company, authority or person), exactly as named in the document, short everyday form without legal form or address, e.g. "Stadtwerke Musterstadt", "Finanzamt Musterstadt", "Allianz". NEVER the recipient the document is addressed to. Always answer with the sender from the document, even if it is not in the list of known senders. Use a known sender's spelling only when it is clearly the same organisation. Empty string only if the document names no sender at all.
type: what kind of document it is, in the document's language, one or two words (e.g. Rechnung, Vertrag, Bescheid, Kontoauszug, Lohnabrechnung, Mahnung, Brief, Invoice).
title: a short descriptive title in the document's language, 2 to 6 words, without the sender's name, e.g. "Jahresrechnung Strom 2025", "Einkommensteuerbescheid 2023".
date: the date the document was issued as YYYY-MM-DD, or empty string.`

var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"sender": map[string]string{"type": "string"},
		"type":   map[string]string{"type": "string"},
		"title":  map[string]string{"type": "string"},
		"date":   map[string]string{"type": "string"},
	},
	"required": []string{"sender", "type", "title", "date"},
}

// maxText keeps the prompt small: the first page or two say who sent it.
const maxText = 4000

func limitList(names []string, n int) string {
	if len(names) > n {
		names = names[:n]
	}
	if len(names) == 0 {
		return "(none yet)"
	}
	return strings.Join(names, ", ")
}

// Suggest reads the document text and returns the model's guess. Known
// senders help it reuse your spelling. Types are not sent: the list made
// small models force documents into ill-fitting types, so the caller maps
// the free answer onto existing types instead.
func (c *Client) Suggest(ctx context.Context, text string, senders []string) (Suggestion, error) {
	var out Suggestion
	if len(text) > maxText {
		text = text[:maxText]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	if strings.TrimSpace(text) == "" {
		return out, nil
	}
	user := "Known senders (for spelling only): " + limitList(senders, 300) + "\n\nDocument text:\n" + text
	body, _ := json.Marshal(map[string]any{
		"model":      c.Model,
		"stream":     false,
		"format":     schema,
		"keep_alive": "15m",
		"options":    map[string]any{"temperature": 0},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, fmt.Errorf("asking %s: %w", c.Model, err)
	}
	defer resp.Body.Close()
	var r struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return out, fmt.Errorf("reading answer from %s: %w", c.Model, err)
	}
	if r.Error != "" {
		return out, fmt.Errorf("%s: %s", c.Model, r.Error)
	}
	if err := json.Unmarshal([]byte(r.Message.Content), &out); err != nil {
		return out, fmt.Errorf("%s gave no usable answer: %w", c.Model, err)
	}
	out.Sender = clean(out.Sender, 80)
	out.Type = clean(out.Type, 40)
	out.Title = clean(out.Title, 100)
	out.Date = strings.TrimSpace(out.Date)
	if _, err := time.Parse("2006-01-02", out.Date); err != nil || out.Date > time.Now().AddDate(0, 0, 1).Format("2006-01-02") {
		out.Date = ""
	}
	return out, nil
}

func clean(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, `"'.,;: `)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// Check reports whether Ollama is reachable and has the model.
func (c *Client) Check(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/tags", nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Ollama at %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	var have []string
	for _, m := range r.Models {
		if m.Name == c.Model || m.Name == c.Model+":latest" {
			return nil
		}
		have = append(have, m.Name)
	}
	return fmt.Errorf("model %q is not installed in Ollama (run: ollama pull %s; installed: %s)", c.Model, c.Model, strings.Join(have, ", "))
}
