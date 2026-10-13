package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeEmbedServer answers batchEmbedContents with vectors whose first value is the text's
// length, after failing the first `fail` calls with 429.
func fakeEmbedServer(t *testing.T, fail int32, seen *[]string) (*Embedder, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/"+EmbedModel+":batchEmbedContents" || r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("unexpected request %s key=%q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		if n <= fail {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var body struct {
			Requests []struct {
				Model   string   `json:"model"`
				Content gContent `json:"content"`
				Dims    int      `json:"outputDimensionality"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		type emb struct {
			Values []float32 `json:"values"`
		}
		var out struct {
			Embeddings []emb `json:"embeddings"`
		}
		for _, q := range body.Requests {
			if q.Model != "models/"+EmbedModel || q.Dims != EmbedDims {
				t.Errorf("request %+v", q)
			}
			*seen = append(*seen, q.Content.Parts[0].Text)
			v := make([]float32, EmbedDims)
			v[0] = float32(len(q.Content.Parts[0].Text))
			out.Embeddings = append(out.Embeddings, emb{v})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	e := NewEmbedder("k")
	e.g.baseURL = srv.URL + "/"
	return e, &calls
}

func TestEmbedDocsBatchesInOrderWithPrefixes(t *testing.T) {
	var seen []string
	e, calls := fakeEmbedServer(t, 0, &seen)
	docs := make([]Doc, 150)
	for i := range docs {
		docs[i] = Doc{Title: "L1", Text: strings.Repeat("x", i)}
	}
	docs[0].Title = ""
	vs, err := e.EmbedDocs(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(vs) != 150 {
		t.Fatalf("calls %d, vectors %d: want 2 batches, 150 vectors", calls.Load(), len(vs))
	}
	prefix := len("title: L1 | text: ")
	if vs[149][0] != float32(prefix+149) {
		t.Errorf("order lost: %v", vs[149][0])
	}
	if seen[0] != "title: none | text: " || !strings.HasPrefix(seen[1], "title: L1 | text: ") {
		t.Errorf("document prefix: %q, %q", seen[0], seen[1])
	}
}

func TestEmbedQueryRetriesRateLimit(t *testing.T) {
	var seen []string
	e, calls := fakeEmbedServer(t, 1, &seen)
	v, err := e.EmbedQuery(context.Background(), "what is a heap")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(v) != EmbedDims || seen[0] != "task: question answering | query: what is a heap" {
		t.Errorf("calls %d, dims %d, text %q", calls.Load(), len(v), seen)
	}
}

func TestEmbedGivesUpAndNilWithoutKey(t *testing.T) {
	var seen []string
	e, _ := fakeEmbedServer(t, 100, &seen)
	if _, err := e.EmbedQuery(context.Background(), "q"); err == nil {
		t.Error("want an error after the retries")
	}
	if NewEmbedder("") != nil {
		t.Error("no key, no embedder")
	}
}
