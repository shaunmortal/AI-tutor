// Package ai is the only package that imports the Anthropic SDK. Everything Claude-specific
// lives here: model ids, token limits, beta flags, response shapes. Callers ask for JSON
// or text and get back a plain result or an *httpx.Error.
//
// Three details worth knowing:
//
//  1. Structured outputs: output_config.format constrains generation to our JSON Schema,
//     so malformed JSON is rare. Callers still validate — a schema-shaped answer can be
//     wrong about the subject.
//  2. Refusals arrive as HTTP 200 with stop_reason "refusal". Check it before content.
//  3. Server-side fallbacks: a declined request is re-run on another model in the same
//     call instead of handing us the refusal.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// Quality says how much capability a call needs, in terms of the work, not a model name.
//
//	High      the answer becomes stored data (a question's answer key, a curriculum):
//	          a wrong answer key marks a learner wrong for being right. Opus.
//	Standard  the answer is read by a person who can judge it, and is asked often
//	          (tutor explanations). Sonnet.
type Quality int

const (
	Standard Quality = iota
	High
)

var models = map[Quality]struct {
	id     string
	effort anthropic.BetaOutputConfigEffort
}{
	// Opus 5.5 defaults to medium effort; stored data is worth the deeper pass.
	High:     {id: "claude-opus-5-5", effort: anthropic.BetaOutputConfigEffortHigh},
	Standard: {id: "claude-sonnet-5-5"},
}

const (
	defaultMaxTokens = 8000
	// A generation call that has not answered in 90 seconds is not going to.
	requestTimeout = 90 * time.Second
)

// Attachment is a file the model should look at alongside the prompt.
type Attachment struct {
	PDF       bool   // false means image
	MediaType string // image/png, image/jpeg, image/gif, image/webp (ignored for PDFs)
	Base64    string
	Label     string // shown just before the file, so the model can refer to it ("slide 4")
}

// Turn is an earlier message in a conversation, oldest first.
type Turn struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

type Request struct {
	System      string
	Prompt      string
	MaxTokens   int
	Quality     Quality
	History     []Turn
	Attachments []Attachment
	// Schema constrains the reply to JSON (any JSON-marshalable schema). Nil means free text.
	Schema any
}

type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

type Response struct {
	Text  string
	Usage Usage
	Model string
}

type Client struct {
	sdk *anthropic.Client
}

// New returns nil when apiKey is empty: the server still starts, and only AI routes fail.
func New(apiKey string) *Client {
	if apiKey == "" {
		return nil
	}
	c := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithRequestTimeout(requestTimeout),
		option.WithMaxRetries(2), // 408/409/429/5xx and dropped connections
	)
	return &Client{sdk: &c}
}

// JSON asks for a reply matching req.Schema and decodes it into out. Quality defaults
// to the caller's choice; most structured output becomes stored data, so pass High.
func (c *Client) JSON(ctx context.Context, req Request, out any) (Usage, error) {
	if req.Schema == nil {
		return Usage{}, errors.New("ai.JSON: Schema is required")
	}
	res, err := c.send(ctx, req)
	if err != nil {
		return Usage{}, err
	}
	if err := json.Unmarshal([]byte(res.Text), out); err != nil {
		return res.Usage, malformed("The model returned something that is not valid JSON.")
	}
	return res.Usage, nil
}

// Text asks for free text a person will read.
func (c *Client) Text(ctx context.Context, req Request) (Response, error) {
	req.Schema = nil
	return c.send(ctx, req)
}

func (c *Client) send(ctx context.Context, req Request) (Response, error) {
	if c == nil {
		return Response{}, httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", "AI features are not configured on this server.")
	}
	m := models[req.Quality]
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	messages := make([]anthropic.BetaMessageParam, 0, len(req.History)+1)
	for _, t := range req.History {
		role := anthropic.BetaMessageParamRoleUser
		if t.Role == "assistant" {
			role = anthropic.BetaMessageParamRoleAssistant
		}
		messages = append(messages, anthropic.BetaMessageParam{
			Role: role, Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(t.Content)},
		})
	}
	messages = append(messages, anthropic.NewBetaUserMessage(userContent(req.Prompt, req.Attachments)...))

	params := anthropic.BetaMessageNewParams{
		Model:     m.id,
		MaxTokens: int64(maxTokens),
		System:    []anthropic.BetaTextBlockParam{{Text: req.System}},
		Messages:  messages,
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
	}
	params.OutputConfig.Effort = m.effort
	if req.Schema != nil {
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: req.Schema}
	}

	msg, err := c.sdk.Beta.Messages.New(ctx, params)
	if err != nil {
		return Response{}, translate(err)
	}
	// A refusal is a 200. Check it before touching content.
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return Response{}, httpx.E(http.StatusServiceUnavailable, "AI_REFUSED", "The model declined this request.")
	case anthropic.BetaStopReasonMaxTokens:
		// Half a JSON document parses to nothing useful.
		return Response{}, malformed("The answer was cut off before it finished.")
	}
	// With thinking on, the reply holds more than one kind of block: take the text one.
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok && t.Text != "" {
			return Response{
				Text:  t.Text,
				Usage: Usage{InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens},
				Model: string(msg.Model),
			}, nil
		}
	}
	return Response{}, malformed("The model returned an empty answer.")
}

// userContent puts files first and the prompt after them, which is where Claude reads best.
func userContent(prompt string, files []Attachment) []anthropic.BetaContentBlockParamUnion {
	var blocks []anthropic.BetaContentBlockParamUnion
	for _, f := range files {
		if f.Label != "" {
			blocks = append(blocks, anthropic.NewBetaTextBlock(f.Label))
		}
		if f.PDF {
			blocks = append(blocks, anthropic.NewBetaDocumentBlock(anthropic.BetaBase64PDFSourceParam{Data: f.Base64}))
		} else {
			blocks = append(blocks, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
				Data: f.Base64, MediaType: anthropic.BetaBase64ImageSourceMediaType(f.MediaType),
			}))
		}
	}
	return append(blocks, anthropic.NewBetaTextBlock(prompt))
}

func malformed(msg string) error {
	return httpx.E(http.StatusServiceUnavailable, "AI_MALFORMED", msg)
}

// translate turns SDK errors into ours. The status stays in the log, out of the message:
// a learner can't act on "401", only on whether waiting will help.
func translate(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return httpx.E(http.StatusGatewayTimeout, "AI_TIMEOUT", "The model took too long.")
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		slog.Error("anthropic api error", "status", apiErr.StatusCode, "err", apiErr.Error())
		switch {
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return httpx.E(http.StatusTooManyRequests, "AI_RATE_LIMITED", "Too many AI requests right now. Try again shortly.")
		case apiErr.StatusCode >= 500:
			return httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", "The AI is busy right now. Try again in a moment.")
		}
		return httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", "The AI is unavailable at the moment. Everything else still works.")
	}
	slog.Error("ai request failed", "err", err)
	return httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", "Could not reach the model.")
}
