package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSpans(t *testing.T) {
	for _, c := range []struct {
		length float64
		want   []span
	}{
		{0, []span{{0, 0}}},
		{599, []span{{0, 0}}},
		{600, []span{{0, 0}}},
		{601, []span{{0, 600}, {600, 0}}},
		{1500, []span{{0, 600}, {600, 1200}, {1200, 0}}},
	} {
		if got := spans(c.length); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("spans(%v) = %v, want %v", c.length, got, c.want)
		}
	}
}

func TestKeep(t *testing.T) {
	segs := []rawSegment{
		{595, 601, "strayed back"}, // starts before the span: the previous one has it
		{600.4, 603.2, " First. "},
		{610, 609, "end before start"},
		{700, 705, "   "},
		{1199.9, 1204, "last"},
		{1200, 1205, "next span's"},
	}
	got := keep(segs, span{600, 1200})
	want := "[{600 604 First.} {610 610 end before start} {1199 1204 last}]"
	if fmt.Sprint(got) != want {
		t.Fatalf("keep = %v", got)
	}
	if got := keep([]rawSegment{{5000, 5001, "open-ended"}}, span{1200, 0}); len(got) != 1 {
		t.Fatalf("open span dropped %v", got)
	}
}

func reply(text, finish string) []byte {
	b, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{"text": text}}}, "finishReason": finish}}})
	return b
}

func TestParseTranscript(t *testing.T) {
	segs, err := parseTranscript(reply(`{"segments":[{"startSec":1.5,"endSec":3,"text":"hi"}]}`, "STOP"))
	if err != nil || len(segs) != 1 || segs[0].StartSec != 1.5 || segs[0].Text != "hi" {
		t.Fatalf("%v %v", segs, err)
	}
	if _, err := parseTranscript(reply(`{"segments":[`, "MAX_TOKENS")); err == nil || errors.Is(err, ErrMedia) {
		t.Fatalf("cut off should be a retryable error, got %v", err)
	}
	if _, err := parseTranscript(reply(``, "SAFETY")); !errors.Is(err, ErrMedia) {
		t.Fatalf("safety should be ErrMedia, got %v", err)
	}
	if _, err := parseTranscript(reply(`not json`, "STOP")); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

// fakeGemini is the Files API, countTokens and generateContent, enough for one transcription.
type fakeGemini struct {
	mu          sync.Mutex
	audioTokens int
	polls       int
	uploaded    []byte
	ranges      []string
	deleted     bool
	countStatus int
}

func (f *fakeGemini) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("x-goog-api-key") != "k" {
		http.Error(w, "no key", 401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/upload/v1beta/files" && r.Header.Get("X-Goog-Upload-Command") == "start":
		if r.Header.Get("X-Goog-Upload-Header-Content-Type") != "audio/mp4" || r.Header.Get("X-Goog-Upload-Header-Content-Length") != "5" {
			http.Error(w, "bad start", 400)
			return
		}
		w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/resumable?id=1")
	case r.URL.Path == "/resumable":
		if r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" {
			http.Error(w, "bad upload", 400)
			return
		}
		f.uploaded = body
		fmt.Fprint(w, `{"file":{"name":"files/abc","uri":"https://files/abc","state":"PROCESSING"}}`)
	case r.Method == "GET" && r.URL.Path == "/v1beta/files/abc":
		f.polls++
		state := "PROCESSING"
		if f.polls >= 2 {
			state = "ACTIVE"
		}
		fmt.Fprintf(w, `{"name":"files/abc","uri":"https://files/abc","state":%q}`, state)
	case r.Method == "DELETE" && r.URL.Path == "/v1beta/files/abc":
		f.deleted = true
		fmt.Fprint(w, `{}`)
	case strings.HasSuffix(r.URL.Path, ":countTokens"):
		if f.countStatus != 0 {
			http.Error(w, "nope", f.countStatus)
			return
		}
		fmt.Fprintf(w, `{"totalTokens":9,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":%d}]}`, f.audioTokens)
	case strings.HasSuffix(r.URL.Path, ":generateContent"):
		var req gRequest
		_ = json.Unmarshal(body, &req)
		p := req.Contents[0].Parts[0]
		if p.FileData == nil || p.VideoMetadata == nil || req.GenerationConfig.ResponseJSONSchema == nil {
			http.Error(w, "bad generate", 400)
			return
		}
		f.ranges = append(f.ranges, p.VideoMetadata.StartOffset+"-"+p.VideoMetadata.EndOffset)
		// Each range answers with one segment in it and one strayed into the next.
		start := 0
		fmt.Sscanf(p.VideoMetadata.StartOffset, "%ds", &start)
		seg := fmt.Sprintf(`{"segments":[{"startSec":%d.5,"endSec":%d,"text":"at %d"},{"startSec":%d,"endSec":%d,"text":"stray"}]}`,
			start, start+4, start, start+600, start+605)
		if p.VideoMetadata.EndOffset == "" {
			seg = fmt.Sprintf(`{"segments":[{"startSec":%d,"endSec":%d,"text":"at %d"}]}`, start+1, start+3, start+1)
		}
		w.Write(reply(seg, "STOP"))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 404)
	}
}

func TestTranscribeFileFake(t *testing.T) {
	filePoll = 0
	f := &fakeGemini{audioTokens: 32 * 700} // 700 s: two ranges
	srv := httptest.NewServer(f)
	defer srv.Close()
	segs, err := newTranscriber("k", srv.URL).TranscribeFile(context.Background(), []byte("audio"), "audio/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(segs); got != "[{0 4 at 0} {601 603 at 601}]" {
		t.Fatalf("segments %s", got)
	}
	if string(f.uploaded) != "audio" || f.polls != 2 || !f.deleted || fmt.Sprint(f.ranges) != "[0s-600s 600s-]" {
		t.Fatalf("uploaded %q polls %d deleted %v ranges %v", f.uploaded, f.polls, f.deleted, f.ranges)
	}
}

func TestTranscribeMediaErrors(t *testing.T) {
	filePoll = 0
	for _, c := range []struct {
		f    *fakeGemini
		want string
	}{
		{&fakeGemini{audioTokens: 0}, "no sound"},
		{&fakeGemini{audioTokens: 32 * (MaxMediaSec + 60)}, "longer than three hours"},
		{&fakeGemini{countStatus: 403}, "must be public"},
	} {
		srv := httptest.NewServer(c.f)
		_, err := newTranscriber("k", srv.URL).TranscribeURL(context.Background(), "https://www.youtube.com/watch?v=x")
		srv.Close()
		if !errors.Is(err, ErrMedia) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want ErrMedia %q, got %v", c.want, err)
		}
	}
	// A busy API is not the recording's fault: retryable.
	srv := httptest.NewServer(&fakeGemini{countStatus: 503})
	defer srv.Close()
	if _, err := newTranscriber("k", srv.URL).TranscribeURL(context.Background(), "u"); err == nil || errors.Is(err, ErrMedia) {
		t.Fatalf("503 should be retryable, got %v", err)
	}
}

// Transcribes a spoken sentence with the real API. Needs macOS say/afconvert, GEMINI_LIVE=1, GEMINI_API_KEY.
func TestTranscribeLive(t *testing.T) {
	if os.Getenv("GEMINI_LIVE") == "" {
		t.Skip("set GEMINI_LIVE=1 to call the real API")
	}
	dir := t.TempDir()
	aiff, m4a := filepath.Join(dir, "x.aiff"), filepath.Join(dir, "x.m4a")
	if out, err := exec.Command("say", "-o", aiff, "binary search halves the interval").CombinedOutput(); err != nil {
		t.Skipf("say: %v %s", err, out)
	}
	if out, err := exec.Command("afconvert", "-f", "m4af", "-d", "aac", aiff, m4a).CombinedOutput(); err != nil {
		t.Fatalf("afconvert: %v %s", err, out)
	}
	data, err := os.ReadFile(m4a)
	if err != nil {
		t.Fatal(err)
	}
	segs, err := NewTranscriber(os.Getenv("GEMINI_API_KEY")).TranscribeFile(context.Background(), data, "audio/mp4")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, s := range segs {
		text.WriteString(s.Text + " ")
	}
	got := strings.ToLower(text.String())
	t.Logf("segments: %+v", segs)
	if !strings.Contains(got, "binary search") || !strings.Contains(got, "interval") || segs[0].StartSec > 2 {
		t.Fatalf("transcript %q", got)
	}
}
