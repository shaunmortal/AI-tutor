package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// Calls the real Gemini API (free tier). Run with GEMINI_LIVE=1 and GEMINI_API_KEY set.
func TestGeminiLive(t *testing.T) {
	if os.Getenv("GEMINI_LIVE") == "" {
		t.Skip("set GEMINI_LIVE=1 to call the real API")
	}
	g := &gemini{key: os.Getenv("GEMINI_API_KEY"), http: &http.Client{Timeout: 90 * time.Second}, models: map[Quality]string{High: "gemini-flash-latest", Standard: "gemini-flash-latest"}}
	ctx := context.Background()

	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"answer"},
		"properties": map[string]any{"answer": map[string]any{"type": "integer"}},
	}
	res, err := g.send(ctx, Request{System: "You are a calculator.", Prompt: "What is 17 + 25?", Quality: High, Schema: schema, MaxTokens: 2000})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var out struct{ Answer int }
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.Answer != 42 {
		t.Fatalf("json reply %q (%v)", res.Text, err)
	}

	res, err = g.send(ctx, Request{System: "Reply in one short sentence.", Prompt: "Say hello in Hindi.", MaxTokens: 2000,
		History: []Turn{{Role: "user", Content: "Hi"}, {Role: "assistant", Content: "Hello!"}}})
	if err != nil || res.Text == "" {
		t.Fatalf("text: %v %q", err, res.Text)
	}
	t.Logf("model %s: %q (tokens in/out %d/%d)", res.Model, res.Text, res.Usage.InputTokens, res.Usage.OutputTokens)
}
