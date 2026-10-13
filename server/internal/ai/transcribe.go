package ai

// Speech to text with Gemini (free tier), for lectures that come without captions: an uploaded
// recording, or a public YouTube video. Works whichever provider answers the tutor.
//
// How:
//  1. An uploaded recording goes to the Files API (resumable upload, polled until ACTIVE) and is
//     deleted afterwards; a YouTube link is passed to the model as it is, nothing is downloaded.
//  2. countTokens gives the recording's length: audio is tokenized at a fixed rate per second.
//  3. generateContent transcribes one time range per call (rangeSec) into a JSON schema, so the
//     output never nears the model's output limit however long the lecture; the ranges are merged.
//
// Measured live (gemini-3.5-flash-lite, Oct 2026): timestamps come back from the start of the
// whole recording even when the media is clipped to the range; a clip of an uploaded audio file
// is ignored (the model hears all of it), a clip of a video is honoured; a clip that starts past
// the end of a YouTube video is a 500, and past the end of audio the model invents speech, which
// is why the length is measured instead of asked for.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/sources"
)

const (
	geminiRoot = "https://generativelanguage.googleapis.com"
	// transcribeModel: the free tier's fast model; Flash itself is often busy (503).
	transcribeModel = "gemini-3.5-flash-lite"
	// rangeSec per call: ten minutes of speech is ~1,500 words, a few thousand output tokens.
	rangeSec = 600
	// audioTokensPerSec is Gemini's documented audio rate. Some files count fewer (25/s was
	// measured for an m4a), so length = tokens/32 is a lower bound; the last range is left
	// open-ended so the tail is never lost.
	audioTokensPerSec = 32
	// MaxMediaSec: three hours, a long lecture. Past that, ask for the recording in parts.
	MaxMediaSec = 3 * 60 * 60
)

// filePoll is how often an uploaded file is checked for ACTIVE. A var so tests don't wait.
var filePoll = 2 * time.Second

// ErrMedia is a recording that can't be transcribed (unreadable, private, no speech); retrying
// won't help. The error text says which.
var ErrMedia = errors.New("media cannot be transcribed")

type mediaErr string

func (e mediaErr) Error() string        { return string(e) }
func (e mediaErr) Is(target error) bool { return target == ErrMedia }

// Transcriber turns speech into timed segments. Nil without a Gemini key.
type Transcriber struct {
	g    *gemini
	root string
}

// NewTranscriber returns nil without a key.
func NewTranscriber(geminiKey string) *Transcriber {
	if geminiKey == "" {
		return nil
	}
	return newTranscriber(geminiKey, geminiRoot)
}

func newTranscriber(key, root string) *Transcriber {
	// Five minutes: a 50 MB upload on a slow link, or a long range of video.
	return &Transcriber{g: &gemini{key: key, baseURL: root + "/v1beta/models/", http: &http.Client{Timeout: 5 * time.Minute}}, root: root}
}

// TranscribeFile transcribes an uploaded recording. mime is Gemini's name for its type.
func (t *Transcriber) TranscribeFile(ctx context.Context, data []byte, mime string) ([]sources.TranscriptSegment, error) {
	f, err := t.upload(ctx, data, mime)
	if err != nil {
		return nil, err
	}
	// The Files API would delete it after 48 h anyway; a lecture should not sit there that long.
	defer t.call(context.WithoutCancel(ctx), http.MethodDelete, t.root+"/v1beta/"+f.Name, nil, nil)
	if err := t.waitActive(ctx, &f); err != nil {
		return nil, err
	}
	return t.transcribe(ctx, gFileData{MimeType: mime, FileURI: f.URI}, strings.HasPrefix(mime, "video/"))
}

// TranscribeURL transcribes a public YouTube video without downloading it.
func (t *Transcriber) TranscribeURL(ctx context.Context, youtubeURL string) ([]sources.TranscriptSegment, error) {
	return t.transcribe(ctx, gFileData{FileURI: youtubeURL}, true)
}

type gFile struct {
	Name  string `json:"name"`
	URI   string `json:"uri"`
	State string `json:"state"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// call is one Files API / countTokens request; 4xx and 5xx come back as errors with the status.
func (t *Transcriber) call(ctx context.Context, method, url string, header map[string]string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("x-goog-api-key", t.g.key)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := t.g.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return res, raw, &statusErr{res.StatusCode, fmt.Sprintf("gemini %s %s: %d %s", method, url, res.StatusCode, clip(string(raw), 300))}
	}
	return res, raw, nil
}

type statusErr struct {
	status int
	msg    string
}

func (e *statusErr) Error() string { return e.msg }

// upload is the Files API's resumable protocol in its two requests: start, then upload+finalize.
func (t *Transcriber) upload(ctx context.Context, data []byte, mime string) (gFile, error) {
	res, _, err := t.call(ctx, http.MethodPost, t.root+"/upload/v1beta/files", map[string]string{
		"X-Goog-Upload-Protocol":              "resumable",
		"X-Goog-Upload-Command":               "start",
		"X-Goog-Upload-Header-Content-Length": strconv.Itoa(len(data)),
		"X-Goog-Upload-Header-Content-Type":   mime,
		"Content-Type":                        "application/json",
	}, []byte(`{"file":{"displayName":"lecture"}}`))
	if err != nil {
		return gFile{}, err
	}
	to := res.Header.Get("X-Goog-Upload-URL")
	if to == "" {
		return gFile{}, errors.New("gemini files: no upload URL")
	}
	_, raw, err := t.call(ctx, http.MethodPost, to, map[string]string{
		"X-Goog-Upload-Offset": "0", "X-Goog-Upload-Command": "upload, finalize",
	}, data)
	if err != nil {
		return gFile{}, err
	}
	var out struct {
		File gFile `json:"file"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.File.Name == "" {
		return gFile{}, fmt.Errorf("gemini files: unexpected reply %s", clip(string(raw), 300))
	}
	return out.File, nil
}

// waitActive polls until Gemini has processed the file (videos take a while).
func (t *Transcriber) waitActive(ctx context.Context, f *gFile) error {
	for {
		switch f.State {
		case "ACTIVE":
			return nil
		case "FAILED":
			msg := "the file could not be processed"
			if f.Error != nil && f.Error.Message != "" {
				msg = f.Error.Message
			}
			return mediaErr("Could not read this recording (" + msg + ").")
		}
		select {
		case <-time.After(filePoll):
		case <-ctx.Done():
			return ctx.Err()
		}
		_, raw, err := t.call(ctx, http.MethodGet, t.root+"/v1beta/"+f.Name, nil, nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, f); err != nil {
			return err
		}
	}
}

// lengthSec is a lower bound on the recording's length, from its audio tokens.
func (t *Transcriber) lengthSec(ctx context.Context, media gFileData) (float64, error) {
	body, _ := json.Marshal(map[string]any{"contents": []gContent{{Parts: []gPart{{FileData: &media}}}}})
	_, raw, err := t.call(ctx, http.MethodPost, t.g.baseURL+transcribeModel+":countTokens",
		map[string]string{"Content-Type": "application/json"}, body)
	var se *statusErr
	if errors.As(err, &se) && se.status >= 400 && se.status < 500 && se.status != http.StatusTooManyRequests {
		// A private, deleted or unsupported video, or a file Gemini can't read.
		return 0, mediaErr("Gemini could not open this recording; it must be public.")
	}
	if err != nil {
		return 0, err
	}
	var out struct {
		PromptTokensDetails []struct {
			Modality   string `json:"modality"`
			TokenCount int    `json:"tokenCount"`
		} `json:"promptTokensDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	for _, d := range out.PromptTokensDetails {
		if d.Modality == "AUDIO" && d.TokenCount > 0 {
			return float64(d.TokenCount) / audioTokensPerSec, nil
		}
	}
	return 0, mediaErr("This recording has no sound to transcribe.")
}

type span struct{ from, to int } // to == 0: to the end of the recording

// spans cuts [0, length) into rangeSec pieces; the last one is open-ended (length is a lower bound).
func spans(length float64) []span {
	var out []span
	from := 0
	for ; float64(from+rangeSec) < length; from += rangeSec {
		out = append(out, span{from, from + rangeSec})
	}
	return append(out, span{from, 0})
}

func (t *Transcriber) transcribe(ctx context.Context, media gFileData, video bool) ([]sources.TranscriptSegment, error) {
	length, err := t.lengthSec(ctx, media)
	if err != nil {
		return nil, err
	}
	if length > MaxMediaSec {
		return nil, mediaErr("This recording is longer than three hours; add it in parts.")
	}
	var all []sources.TranscriptSegment
	for _, sp := range spans(length) {
		segs, err := t.transcribeSpan(ctx, media, video, sp)
		if err != nil {
			return nil, err
		}
		all = append(all, keep(segs, sp)...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].StartSec < all[j].StartSec })
	if len(all) == 0 {
		return nil, mediaErr("No speech was found in this recording.")
	}
	return all, nil
}

type rawSegment struct {
	StartSec float64 `json:"startSec"`
	EndSec   float64 `json:"endSec"`
	Text     string  `json:"text"`
}

var transcriptSchema = map[string]any{
	"type": "object", "required": []string{"segments"},
	"properties": map[string]any{"segments": map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object", "required": []string{"startSec", "endSec", "text"},
			"properties": map[string]any{
				"startSec": map[string]any{"type": "number"},
				"endSec":   map[string]any{"type": "number"},
				"text":     map[string]any{"type": "string"},
			},
		},
	}},
}

func (t *Transcriber) transcribeSpan(ctx context.Context, media gFileData, video bool, sp span) ([]rawSegment, error) {
	meta := &gVideoMeta{StartOffset: strconv.Itoa(sp.from) + "s"}
	where := fmt.Sprintf("from %ds to the end", sp.from)
	if sp.to > 0 {
		meta.EndOffset = strconv.Itoa(sp.to) + "s"
		where = fmt.Sprintf("from %ds to %ds", sp.from, sp.to)
	}
	if video {
		meta.FPS = 0.2 // the words are in the sound; fewer frames, far fewer tokens
	}
	prompt := "Transcribe the speech in this lecture recording " + where + ", verbatim, in its own language. " +
		"Split it into segments of one or two sentences. startSec and endSec are seconds from the start of the " +
		"whole recording, not of this part. Leave out music and silence. If there is no speech, return no segments."
	body := gRequest{
		Contents: []gContent{{Role: "user", Parts: []gPart{{FileData: &media, VideoMetadata: meta}, {Text: prompt}}}},
		GenerationConfig: gGenerateCfg{MaxOutputTokens: 32000, ResponseMimeType: "application/json",
			ResponseJSONSchema: transcriptSchema},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw, err := t.g.post(ctx, transcribeModel, payload)
	if err != nil {
		return nil, err
	}
	return parseTranscript(raw)
}

// parseTranscript reads a generateContent reply holding the transcript schema.
func parseTranscript(raw []byte) ([]rawSegment, error) {
	var res gResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, malformed("The model returned something unreadable.")
	}
	if res.PromptFeedback != nil && res.PromptFeedback.BlockReason != "" {
		return nil, mediaErr("Gemini declined to transcribe this recording.")
	}
	if len(res.Candidates) == 0 {
		return nil, malformed("The model returned an empty answer.")
	}
	c := res.Candidates[0]
	switch c.FinishReason {
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "RECITATION":
		return nil, mediaErr("Gemini declined to transcribe this recording.")
	case "MAX_TOKENS":
		return nil, malformed("The transcript was cut off before it finished.")
	}
	var text strings.Builder
	for _, p := range c.Content.Parts {
		text.WriteString(p.Text)
	}
	var out struct {
		Segments []rawSegment `json:"segments"`
	}
	if err := json.Unmarshal([]byte(text.String()), &out); err != nil {
		return nil, malformed("The model returned a transcript that is not valid JSON.")
	}
	return out.Segments, nil
}

// keep turns a span's segments into whole-second ones, dropping blanks and any that start
// outside the span (a model that strays past its range would otherwise duplicate the next one).
func keep(segs []rawSegment, sp span) []sources.TranscriptSegment {
	var out []sources.TranscriptSegment
	for _, s := range segs {
		text := strings.TrimSpace(s.Text)
		if text == "" || s.StartSec < float64(sp.from) || (sp.to > 0 && s.StartSec >= float64(sp.to)) {
			continue
		}
		start := int(math.Floor(s.StartSec))
		out = append(out, sources.TranscriptSegment{StartSec: start, EndSec: max(start, int(math.Ceil(s.EndSec))), Text: text})
	}
	return out
}
