package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeOllama answers /api/chat with the given content and records the request.
func fakeOllama(t *testing.T, answer string, got *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			if got != nil {
				json.NewDecoder(r.Body).Decode(got)
			}
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": answer}})
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "qwen2.5:7b"}, {"name": "llama3:latest"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSuggestRequestAndAnswer(t *testing.T) {
	var req map[string]any
	srv := fakeOllama(t, `{"sender":" \"Stadtwerke Musterstadt\". ","type":"Rechnung","title":"Jahresrechnung Strom 2025","date":"2026-03-11"}`, &req)
	c := New(srv.URL+"/", "qwen2.5:7b")

	s, err := c.Suggest(context.Background(), "Stadtwerke Musterstadt GmbH\nRechnung", []string{"Finanzamt", "Allianz"})
	if err != nil {
		t.Fatal(err)
	}
	want := Suggestion{Sender: "Stadtwerke Musterstadt", Type: "Rechnung", Title: "Jahresrechnung Strom 2025", Date: "2026-03-11"}
	if s != want {
		t.Errorf("got %+v, want %+v", s, want)
	}

	if req["model"] != "qwen2.5:7b" || req["stream"] != false {
		t.Errorf("model/stream not sent right: %v %v", req["model"], req["stream"])
	}
	if _, ok := req["format"].(map[string]any); !ok {
		t.Error("no JSON schema sent as format")
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want system and user message, got %d", len(msgs))
	}
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "Finanzamt, Allianz") || !strings.Contains(user, "Stadtwerke Musterstadt GmbH") {
		t.Errorf("user message misses known senders or text:\n%s", user)
	}
	if strings.Contains(user, "Known types") {
		t.Error("known types must not be sent (they bias small models)")
	}
}

func TestSuggestDropsBadDates(t *testing.T) {
	future := time.Now().AddDate(1, 0, 0).Format("2006-01-02")
	for _, date := range []string{"next week", "31.12.2024", future} {
		srv := fakeOllama(t, `{"sender":"X","type":"","title":"","date":"`+date+`"}`, nil)
		s, err := New(srv.URL, "m").Suggest(context.Background(), "text", nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Date != "" {
			t.Errorf("date %q should be dropped, got %q", date, s.Date)
		}
	}
}

func TestSuggestLimitsText(t *testing.T) {
	var req map[string]any
	srv := fakeOllama(t, `{"sender":"","type":"","title":"","date":""}`, &req)
	long := strings.Repeat("ä", 10000) // 2 bytes each: cutting must not split a character
	if _, err := New(srv.URL, "m").Suggest(context.Background(), long, nil); err != nil {
		t.Fatal(err)
	}
	user := req["messages"].([]any)[1].(map[string]any)["content"].(string)
	if len(user) > maxText+200 {
		t.Errorf("prompt too long: %d bytes", len(user))
	}
	if !utf8.ValidString(user) {
		t.Error("text was cut inside a character")
	}
}

func TestSuggestEmptyTextSkipsModel(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	if _, err := New(srv.URL, "m").Suggest(context.Background(), "  \n ", nil); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("empty documents shouldn't be sent to the model")
	}
}

func TestSuggestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": `model "nope" not found`})
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "nope").Suggest(context.Background(), "text", nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want the model error passed on, got %v", err)
	}
	bad := fakeOllama(t, `I think the sender is the Stadtwerke.`, nil)
	if _, err := New(bad.URL, "m").Suggest(context.Background(), "text", nil); err == nil {
		t.Error("want an error for a non-JSON answer")
	}
	if _, err := New("http://127.0.0.1:1", "m").Suggest(context.Background(), "text", nil); err == nil {
		t.Error("want an error when Ollama isn't running")
	}
}

func TestCheck(t *testing.T) {
	srv := fakeOllama(t, "", nil)
	ctx := context.Background()
	if err := New(srv.URL, "qwen2.5:7b").Check(ctx); err != nil {
		t.Errorf("installed model: %v", err)
	}
	if err := New(srv.URL, "llama3").Check(ctx); err != nil {
		t.Errorf("model with implicit :latest tag: %v", err)
	}
	if err := New(srv.URL, "mistral").Check(ctx); err == nil || !strings.Contains(err.Error(), "ollama pull mistral") {
		t.Errorf("missing model should say how to get it, got %v", err)
	}
	if err := New("http://127.0.0.1:1", "m").Check(ctx); err == nil {
		t.Error("want an error when Ollama isn't reachable")
	}
}
