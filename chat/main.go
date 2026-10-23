// Command chat is the backend for the "Ask my AI" assistant on https://befika.github.io.
// It answers visitor questions about Befikadu's profile using the Gemini API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	allowedOrigin = "https://befika.github.io"
	maxMessages   = 10   // conversation turns sent to the model
	maxChars      = 1000 // per message
	ratePerWindow = 15   // requests per IP per window
	rateWindow    = 10 * time.Minute
)

var (
	apiKey    = os.Getenv("GEMINI_API_KEY")
	model     = envOr("GEMINI_MODEL", "gemini-3.5-flash-lite")
	geminiURL = "https://generativelanguage.googleapis.com/v1beta/models/" // swapped in tests
	client    = &http.Client{Timeout: 25 * time.Second}
	errBusy   = errors.New("gemini rate limit")
)

type message struct {
	Role string `json:"role"` // "user" or "model"
	Text string `json:"text"`
}

type part struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

func main() {
	if apiKey == "" {
		log.Print("warning: GEMINI_API_KEY is not set; /api/chat will answer 500")
	}
	http.HandleFunc("/api/chat", handleChat)
	http.HandleFunc("/api/health", handleHealth)
	port := envOr("PORT", "3000")
	log.Printf("chat listening on :%s (model %s)", port, model)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func cors(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == allowedOrigin {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
	w.Header().Set("Vary", "Origin")
}

// handleHealth backs the live status on the portfolio; it never calls Gemini.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	cors(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": apiKey != "", "model": model})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cors(w, r)

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if apiKey == "" {
		writeError(w, http.StatusInternalServerError, "The assistant is not configured yet (GEMINI_API_KEY missing).")
		return
	}
	if !allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "Too many questions in a short time. Please try again in a few minutes.")
		return
	}

	var req struct {
		Messages []message `json:"messages"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	msgs, err := validate(req.Messages)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	modelStart := time.Now()
	reply, err := ask(r.Context(), msgs)
	modelMs := time.Since(modelStart).Milliseconds()
	switch {
	case errors.Is(err, errBusy):
		writeError(w, http.StatusServiceUnavailable, "The assistant is busy right now. Please try again in a minute.")
	case err != nil:
		log.Printf("gemini: %v", err)
		writeError(w, http.StatusBadGateway, "The assistant is unavailable right now.")
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"reply": reply,
			// timings drive the trace waterfall on the portfolio
			"trace": map[string]any{"model": model, "model_ms": modelMs, "server_ms": time.Since(start).Milliseconds()},
		})
	}
}

// validate keeps the last maxMessages turns, starting with a user turn and ending with one.
func validate(msgs []message) ([]message, error) {
	if len(msgs) == 0 {
		return nil, errors.New("messages is empty")
	}
	if len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "model" {
			return nil, fmt.Errorf("invalid role %q", m.Role)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(m.Text)); n == 0 || n > maxChars {
			return nil, fmt.Errorf("each message must be 1-%d characters", maxChars)
		}
	}
	if msgs[len(msgs)-1].Role != "user" {
		return nil, errors.New("last message must be from the user")
	}
	for msgs[0].Role != "user" {
		msgs = msgs[1:]
	}
	return msgs, nil
}

func ask(ctx context.Context, msgs []message) (string, error) {
	body := map[string]any{
		"systemInstruction": content{Parts: []part{{Text: systemPrompt}}},
		"generationConfig":  map[string]any{"maxOutputTokens": 1024},
	}
	contents := make([]content, len(msgs))
	for i, m := range msgs {
		contents[i] = content{Role: m.Role, Parts: []part{{Text: m.Text}}}
	}
	body["contents"] = contents

	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiURL+model+":generateContent", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", errBusy
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Candidates []struct {
			Content      content `json:"content"`
			FinishReason string  `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 {
		return "", errors.New("no candidates")
	}
	var sb strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		if !p.Thought {
			sb.WriteString(p.Text)
		}
	}
	reply := strings.TrimSpace(sb.String())
	if reply == "" {
		return "", fmt.Errorf("empty reply (finishReason %s)", out.Candidates[0].FinishReason)
	}
	return reply, nil
}

// ponytail: in-memory, per-instance fixed window; resets on cold start and isn't shared
// across instances. Gemini's free-tier quota is the hard cap behind it. Move to Upstash/Vercel KV if abused.
var limiter = struct {
	sync.Mutex
	start time.Time
	hits  map[string]int
}{hits: map[string]int{}}

func allow(ip string) bool {
	limiter.Lock()
	defer limiter.Unlock()
	if time.Since(limiter.start) > rateWindow {
		limiter.start = time.Now()
		clear(limiter.hits)
	}
	limiter.hits[ip]++
	return limiter.hits[ip] <= ratePerWindow
}

// clientIP trusts X-Real-IP because Vercel's edge sets it; RemoteAddr covers local runs.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
