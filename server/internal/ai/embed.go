package ai

// Embeddings for hybrid retrieval, from the Gemini API (free tier) whichever provider answers.
//
// gemini-embedding-2 ignores the taskType field; the task goes in the text instead, as Google's
// embeddings guide says: "task: question answering | query: …" for a question and
// "title: … | text: …" for an excerpt. Truncated outputs (768 here) come back normalized, so the
// cosine distance in hybrid_search_source_chunks needs nothing more.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const (
	EmbedModel = "gemini-embedding-2"
	// EmbedDims matches source_chunks.embedding vector(768) (migration 029).
	EmbedDims = 768
	// embedBatch is batchEmbedContents' limit on requests per call.
	embedBatch = 100
)

// Embedder turns text into vectors. Nil when there is no Gemini key: callers then search by words.
type Embedder struct{ g *gemini }

// NewEmbedder returns nil without a key.
func NewEmbedder(geminiKey string) *Embedder {
	if geminiKey == "" {
		return nil
	}
	return &Embedder{&gemini{key: geminiKey, http: &http.Client{Timeout: time.Minute}}}
}

// Doc is one excerpt to embed; Title is its source's.
type Doc struct{ Title, Text string }

// EmbedDocs embeds excerpts, batchEmbedContents at a time, in order.
func (e *Embedder) EmbedDocs(ctx context.Context, docs []Doc) ([][]float32, error) {
	texts := make([]string, len(docs))
	for i, d := range docs {
		title := d.Title
		if title == "" {
			title = "none"
		}
		texts[i] = "title: " + title + " | text: " + d.Text
	}
	var out [][]float32
	for start := 0; start < len(texts); start += embedBatch {
		vs, err := e.embed(ctx, texts[start:min(start+embedBatch, len(texts))])
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

// EmbedQuery embeds a learner's question for search.
func (e *Embedder) EmbedQuery(ctx context.Context, question string) ([]float32, error) {
	vs, err := e.embed(ctx, []string{"task: question answering | query: " + question})
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

func (e *Embedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	type request struct {
		Model                string   `json:"model"`
		Content              gContent `json:"content"`
		OutputDimensionality int      `json:"outputDimensionality"`
	}
	body := struct {
		Requests []request `json:"requests"`
	}{}
	for _, t := range texts {
		body.Requests = append(body.Requests, request{Model: "models/" + EmbedModel,
			Content: gContent{Parts: []gPart{{Text: t}}}, OutputDimensionality: EmbedDims})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw, err := e.g.postTo(ctx, EmbedModel+":batchEmbedContents", payload)
	if err != nil {
		return nil, err
	}
	var res struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Embeddings) != len(texts) {
		return nil, malformed("The embedding service returned something unreadable.")
	}
	out := make([][]float32, len(texts))
	for i, v := range res.Embeddings {
		if len(v.Values) != EmbedDims {
			return nil, malformed("The embedding service returned a vector of the wrong size.")
		}
		out[i] = v.Values
	}
	return out, nil
}
