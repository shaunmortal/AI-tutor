package ai

import (
	"context"
	"os"
	"testing"
)

// Calls the real API; costs a few cents. Run with AI_LIVE=1 and ANTHROPIC_API_KEY set.
func TestLive(t *testing.T) {
	if os.Getenv("AI_LIVE") == "" {
		t.Skip("set AI_LIVE=1 to call the real API")
	}
	c := New(Settings{AnthropicKey: os.Getenv("ANTHROPIC_API_KEY")})
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
		t.Fatalf("high JSON answer = %d", out.Answer)
	}

	res, err := c.Text(ctx, Request{System: "Reply in one short sentence.", Prompt: "Say hello.", Quality: Standard, MaxTokens: 2000,
		History: []Turn{{Role: "user", Content: "Hi"}, {Role: "assistant", Content: "Hello!"}}})
	if err != nil || res.Text == "" {
		t.Fatalf("standard text: %v %q", err, res.Text)
	}
	t.Logf("models: %s; text: %q", res.Model, res.Text)
}
