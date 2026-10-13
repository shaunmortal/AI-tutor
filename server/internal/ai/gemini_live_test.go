package ai

import (
	"context"
	"os"
	"testing"
)

// Calls the real Gemini API through Client (free tier). Run with GEMINI_LIVE=1 and GEMINI_API_KEY.
func TestGeminiLive(t *testing.T) {
	if os.Getenv("GEMINI_LIVE") == "" {
		t.Skip("set GEMINI_LIVE=1 to call the real API")
	}
	c := New(Settings{Provider: "gemini", GeminiKey: os.Getenv("GEMINI_API_KEY")})
	ctx := context.Background()

	var out struct {
		Answer int `json:"answer"`
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"answer"},
		"properties": map[string]any{"answer": map[string]any{"type": "integer"}},
	}
	if _, err := c.JSON(ctx, Request{System: "You are a calculator.", Prompt: "What is 17 + 25?", Quality: High, Schema: schema, MaxTokens: 2000}, &out); err != nil {
		t.Fatalf("high JSON: %v", err)
	}
	if out.Answer != 42 {
		t.Fatalf("answer = %d", out.Answer)
	}

	res, err := c.Text(ctx, Request{System: "Reply in one short sentence.", Prompt: "Say hello in Hindi.", MaxTokens: 2000,
		History: []Turn{{Role: "user", Content: "Hi"}, {Role: "assistant", Content: "Hello!"}}})
	if err != nil || res.Text == "" {
		t.Fatalf("standard text: %v %q", err, res.Text)
	}
	t.Logf("model %s: %q (tokens in/out %d/%d)", res.Model, res.Text, res.Usage.InputTokens, res.Usage.OutputTokens)
}
