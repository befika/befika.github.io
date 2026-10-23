package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChat(t *testing.T) {
	var gotPath, gotKey string
	var gotBody struct {
		SystemInstruction content   `json:"systemInstruction"`
		Contents          []content `json:"contents"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-goog-api-key")
		json.NewDecoder(r.Body).Decode(&gotBody)
		if last := gotBody.Contents[len(gotBody.Contents)-1]; last.Parts[0].Text == "busy" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true},{"text":" Yes, 8+ years of Go. "}]}}]}`)
	}))
	defer upstream.Close()
	geminiURL = upstream.URL + "/models/"

	do := func(method, body, origin, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/chat", strings.NewReader(body))
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Real-IP", ip)
		rec := httptest.NewRecorder()
		handleChat(rec, req)
		return rec
	}
	user := func(text string) string { return `{"messages":[{"role":"user","text":"` + text + `"}]}` }

	if rec := do("POST", user("hi"), "", "9.9.9.9"); rec.Code != 500 || !strings.Contains(rec.Body.String(), "GEMINI_API_KEY") {
		t.Errorf("missing key: %d %s", rec.Code, rec.Body)
	}
	apiKey = "test-key"

	rec := do("POST", user("Go experience?"), allowedOrigin, "1.1.1.1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reply":"Yes, 8+ years of Go."`) {
		t.Fatalf("happy path: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != allowedOrigin {
		t.Error("missing CORS header for allowed origin")
	}
	if gotKey != "test-key" || gotPath != "/models/"+model+":generateContent" {
		t.Errorf("upstream got key %q path %q", gotKey, gotPath)
	}
	if !strings.Contains(gotBody.SystemInstruction.Parts[0].Text, "Befikadu Shumet") {
		t.Error("system prompt not sent")
	}

	// 11 alternating turns: keep the last 10, then drop the leading model turn.
	var turns []string
	for i := 0; i <= 10; i++ {
		role := map[bool]string{true: "user", false: "model"}[i%2 == 0]
		turns = append(turns, `{"role":"`+role+`","text":"t"}`)
	}
	if rec := do("POST", `{"messages":[`+strings.Join(turns, ",")+`]}`, "", "1.1.1.1"); rec.Code != 200 || len(gotBody.Contents) != 9 || gotBody.Contents[0].Role != "user" {
		t.Errorf("trimming: %d, sent %d turns", rec.Code, len(gotBody.Contents))
	}

	for _, tc := range []struct {
		name, method, body string
		want               int
	}{
		{"wrong method", "GET", "", 405},
		{"preflight", "OPTIONS", "", 204},
		{"bad json", "POST", `{`, 400},
		{"empty", "POST", `{"messages":[]}`, 400},
		{"bad role", "POST", `{"messages":[{"role":"system","text":"hi"}]}`, 400},
		{"last not user", "POST", `{"messages":[{"role":"user","text":"hi"},{"role":"model","text":"yo"}]}`, 400},
		{"blank", "POST", user("   "), 400},
		{"too long", "POST", user(strings.Repeat("a", maxChars+1)), 400},
		{"upstream busy", "POST", user("busy"), 503},
	} {
		if rec := do(tc.method, tc.body, "", "2.2.2.2"); rec.Code != tc.want {
			t.Errorf("%s: got %d want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
		}
	}

	if rec := do("POST", user("hi"), "https://evil.example", "3.3.3.3"); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS header sent to foreign origin")
	}

	for i := 1; i <= ratePerWindow+1; i++ {
		rec := do("POST", user("hi"), "", "4.4.4.4")
		if want := map[bool]int{true: 200, false: 429}[i <= ratePerWindow]; rec.Code != want {
			t.Fatalf("rate limit request %d: got %d want %d", i, rec.Code, want)
		}
	}
}
