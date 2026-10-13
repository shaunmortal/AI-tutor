package ai

// Gemini as a second provider, over its REST API with the standard library — no SDK needed
// for the one call we make. Selected with AI_PROVIDER=gemini; everything above Client is
// unchanged, so callers never know which model answered.
//
// Differences from Claude worth knowing:
//   - Structured output uses responseJsonSchema, which takes our JSON Schemas as they are.
//   - A safety block arrives as finishReason SAFETY/PROHIBITED_CONTENT (or a blocked prompt),
//     not a refusal stop reason; there is no server-side fallback, so it maps to AI_REFUSED.
//   - The free tier allows few requests per minute; a 429 becomes AI_RATE_LIMITED and the
//     client's retry-with-backoff takes it from there.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const geminiBase = "https://generativelanguage.googleapis.com/v1beta/models/"

// geminiModels mirrors the Claude split: High for stored data, Standard for explanations.
var geminiModels = map[Quality]string{
	High:     "gemini-pro-latest",
	Standard: "gemini-flash-latest",
}

type gemini struct {
	key    string
	http   *http.Client
	models map[Quality]string
}

type gPart struct {
	Text       string   `json:"text,omitempty"`
	InlineData *gInline `json:"inlineData,omitempty"`
}

type gInline struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}

type gRequest struct {
	SystemInstruction *gContent    `json:"systemInstruction,omitempty"`
	Contents          []gContent   `json:"contents"`
	GenerationConfig  gGenerateCfg `json:"generationConfig"`
}

type gGenerateCfg struct {
	MaxOutputTokens    int    `json:"maxOutputTokens,omitempty"`
	ResponseMimeType   string `json:"responseMimeType,omitempty"`
	ResponseJSONSchema any    `json:"responseJsonSchema,omitempty"`
}

type gResponse struct {
	Candidates []struct {
		Content      gContent `json:"content"`
		FinishReason string   `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

func (g *gemini) send(ctx context.Context, req Request) (Response, error) {
	model := g.models[req.Quality]
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	body := gRequest{
		SystemInstruction: &gContent{Parts: []gPart{{Text: req.System}}},
		GenerationConfig:  gGenerateCfg{MaxOutputTokens: maxTokens},
	}
	for _, t := range req.History {
		role := "user"
		if t.Role == "assistant" {
			role = "model"
		}
		body.Contents = append(body.Contents, gContent{Role: role, Parts: []gPart{{Text: t.Content}}})
	}
	// Files first, then the prompt, as for Claude.
	var parts []gPart
	for _, f := range req.Attachments {
		if f.Label != "" {
			parts = append(parts, gPart{Text: f.Label})
		}
		mime := f.MediaType
		if f.PDF {
			mime = "application/pdf"
		}
		parts = append(parts, gPart{InlineData: &gInline{MimeType: mime, Data: f.Base64}})
	}
	body.Contents = append(body.Contents, gContent{Role: "user", Parts: append(parts, gPart{Text: req.Prompt})})
	if req.Schema != nil {
		body.GenerationConfig.ResponseMimeType = "application/json"
		body.GenerationConfig.ResponseJSONSchema = req.Schema
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	raw, err := g.post(ctx, model, payload)
	if err != nil {
		return Response{}, err
	}

	var out gResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, malformed("The model returned something unreadable.")
	}
	if out.PromptFeedback != nil && out.PromptFeedback.BlockReason != "" {
		return Response{}, refused()
	}
	if len(out.Candidates) == 0 {
		return Response{}, malformed("The model returned an empty answer.")
	}
	c := out.Candidates[0]
	switch c.FinishReason {
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "RECITATION":
		return Response{}, refused()
	case "MAX_TOKENS":
		return Response{}, malformed("The answer was cut off before it finished.")
	}
	var text strings.Builder
	for _, p := range c.Content.Parts {
		text.WriteString(p.Text)
	}
	if text.Len() == 0 {
		return Response{}, malformed("The model returned an empty answer.")
	}
	modelName := out.ModelVersion
	if modelName == "" {
		modelName = model
	}
	return Response{
		Text:  text.String(),
		Usage: Usage{InputTokens: out.UsageMetadata.PromptTokenCount, OutputTokens: out.UsageMetadata.CandidatesTokenCount},
		Model: modelName,
	}, nil
}

// geminiRetries matches the Claude SDK's default: busy (503) and rate-limited (429) answers
// are common on the free tier and usually clear within seconds.
const geminiRetries = 2

func (g *gemini) post(ctx context.Context, model string, payload []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiBase+model+":generateContent", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		// Header, not query string, so the key never lands in a proxy or access log.
		hreq.Header.Set("x-goog-api-key", g.key)

		res, err := g.http.Do(hreq)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, timeoutErr()
			}
			slog.Error("gemini request failed", "err", err)
			return nil, unavailable("Could not reach the model.")
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			return raw, nil
		}
		retryable := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		if !retryable || attempt == geminiRetries {
			return nil, geminiStatusErr(res.StatusCode, raw)
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
		case <-ctx.Done():
			return nil, timeoutErr()
		}
	}
}

func geminiStatusErr(status int, body []byte) error {
	slog.Error("gemini api error", "status", status, "body", clip(string(body), 300))
	switch {
	case status == http.StatusTooManyRequests:
		return rateLimited()
	case status >= 500:
		return unavailable("The AI is busy right now. Try again in a moment.")
	}
	return unavailable("The AI is unavailable at the moment. Everything else still works.")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("… (%d bytes)", len(s))
}
