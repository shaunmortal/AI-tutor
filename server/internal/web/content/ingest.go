package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/sources"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

const (
	// Claude reads PDFs up to 100 pages in one request; past that, figures are skipped, text is not.
	maxPDFPagesForFigures = 100
	// Slide pictures described per request.
	imagesPerCall = 16
	// A source left 'processing' this long with no job in the stream lost its job (see recover).
	orphanAfter = time.Minute
)

// job is the queue payload: only the id, everything else is read fresh from the database.
type job struct {
	SourceID string `json:"sourceId"`
}

// failure is a problem with the material itself (not a PDF, no captions): retrying will not
// help, so the source is marked failed at once with this message.
type failure struct{ msg string }

func (f failure) Error() string { return f.msg }

// StartWorkers starts the ingestion consumer (and a one-off sweep for orphaned sources) and
// returns; both stop when ctx is cancelled. Safe on several instances at once.
func StartWorkers(ctx context.Context, d *app.Deps) {
	s := newSvc(d)
	go s.recoverOrphans(ctx)
	go s.queue.Run(ctx, s.ingest)
}

// giveUp is the queue's OnGiveUp: the source says why, instead of spinning for ever.
func (s *svc) giveUp(ctx context.Context, payload []byte, err error) {
	var j job
	if json.Unmarshal(payload, &j) == nil && j.SourceID != "" {
		s.fail(ctx, j.SourceID, "Processing failed after several attempts ("+err.Error()+"). Delete this source and add it again.")
		if found, err := s.sources().FindMany(ctx, []string{j.SourceID}); err == nil && len(found) == 1 {
			s.organizeIfPending(ctx, found[0].UserID)
		}
	}
}

func (s *svc) fail(ctx context.Context, sourceID, msg string) {
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300])
	}
	if err := s.sources().SetStatus(context.WithoutCancel(ctx), sourceID, "failed", &msg); err != nil {
		slog.Error("mark source failed", "source", sourceID, "err", err)
	}
}

// recoverOrphans re-queues sources stuck on 'processing' with no job in the stream: ones
// created by the old in-process ingestion, or whose enqueue was lost. One query and one
// XRANGE at startup. Duplicates are harmless: ingest skips a source that is no longer processing.
func (s *svc) recoverOrphans(ctx context.Context) {
	rows, _ := s.d.DB.Admin.Query(ctx, // its error surfaces from CollectRows
		`select id::text from course_sources where status = 'processing' and created_at < now() - make_interval(secs => $1)`,
		orphanAfter.Seconds())
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		if err != nil {
			slog.Warn("ingest recovery", "err", err)
		}
		return
	}
	queued := map[string]bool{}
	msgs, err := s.d.Redis.XRange(ctx, ingestStream, "-", "+").Result()
	if err != nil {
		slog.Warn("ingest recovery", "err", err)
		return
	}
	for _, m := range msgs {
		var j job
		if json.Unmarshal([]byte(fmt.Sprint(m.Values["p"])), &j) == nil {
			queued[j.SourceID] = true
		}
	}
	for _, id := range ids {
		if !queued[id] {
			slog.Info("ingest recovery: re-queueing", "source", id)
			_ = s.queue.Enqueue(ctx, job{SourceID: id})
		}
	}
}

// ingest is the queue handler. Idempotent: it replaces the source's excerpts in one
// transaction, and does nothing for a source that is gone or already finished.
func (s *svc) ingest(ctx context.Context, payload []byte) error {
	var j job
	if err := json.Unmarshal(payload, &j); err != nil || j.SourceID == "" {
		slog.Error("ingest: bad payload", "payload", string(payload))
		return nil
	}
	found, err := s.sources().FindMany(ctx, []string{j.SourceID})
	if err != nil {
		return err
	}
	if len(found) == 0 || found[0].Status != "processing" {
		return nil
	}
	src := found[0]

	chunks, err := s.extract(ctx, src)
	var f failure
	if errors.As(err, &f) {
		s.fail(ctx, src.ID, f.msg)
		s.organizeIfPending(ctx, src.UserID)
		return nil
	}
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		s.fail(ctx, src.ID, "No readable content was found. Scanned PDFs need OCR first.")
		s.organizeIfPending(ctx, src.UserID)
		return nil
	}
	s.embed(ctx, src, chunks)
	defer s.organizeIfPending(ctx, src.UserID)
	return pgx.BeginFunc(ctx, s.d.DB.Admin, func(tx pgx.Tx) error {
		r := store.NewSources(tx)
		if err := r.DeleteChunks(ctx, src.ID); err != nil {
			return err
		}
		if err := r.InsertChunks(ctx, src, chunks); err != nil {
			return err
		}
		return r.SetStatus(ctx, src.ID, "ready", nil)
	})
}

// embed gives each excerpt its embedding for hybrid search. Not worth failing ingestion over:
// without one an excerpt is still found by its words, and cmd/backfill-embeddings fills it in.
func (s *svc) embed(ctx context.Context, src store.SourceRow, chunks []store.NewChunk) {
	if s.d.Embed == nil {
		return
	}
	docs := make([]ai.Doc, len(chunks))
	for i, c := range chunks {
		docs[i] = ai.Doc{Title: src.Title, Text: c.Content}
	}
	vs, err := s.d.Embed.EmbedDocs(ctx, docs)
	if err != nil {
		slog.Warn("ingest: excerpts not embedded, searchable by words until backfilled", "source", src.ID, "err", err)
		return
	}
	for i := range chunks {
		chunks[i].Embedding = vs[i]
	}
}

// extract turns a source into excerpts. Storage and model errors are returned for a retry;
// problems with the material itself come back as a failure.
func (s *svc) extract(ctx context.Context, src store.SourceRow) ([]store.NewChunk, error) {
	if src.Kind == "video" {
		segments, err := s.videoSegments(ctx, src)
		if err != nil {
			return nil, err
		}
		var chunks []store.NewChunk
		for _, w := range sources.WindowTranscript(segments, sources.TranscriptWindowSec) {
			chunks = append(chunks, store.NewChunk{Modality: "transcript", StartSec: ptr(w.StartSec), EndSec: ptr(w.EndSec), Content: w.Text})
		}
		return chunks, nil
	}

	if src.StoragePath == nil {
		return nil, failure{"The uploaded file is missing."}
	}
	file, err := s.files.download(ctx, *src.StoragePath)
	if err != nil {
		return nil, err
	}
	switch fileFormat(file) {
	case "pdf":
		return s.ingestPDF(ctx, file, src.Kind == "slides")
	case "pptx":
		return s.ingestPPTX(ctx, file)
	}
	return nil, failure{"That file is not a PDF or a .pptx."}
}

// videoSegments: uploaded captions, an uploaded recording (transcribed), or a YouTube video's
// published captions, transcribed by Gemini straight from the link when it has none.
func (s *svc) videoSegments(ctx context.Context, src store.SourceRow) ([]sources.TranscriptSegment, error) {
	if src.StoragePath != nil {
		file, err := s.files.download(ctx, *src.StoragePath)
		if err != nil {
			return nil, err
		}
		if !isMedia(*src.StoragePath) {
			return sources.ParseCaptions(string(file)), nil
		}
		m, ok := mediaFormat(file)
		if !ok || s.tr == nil {
			return nil, failure{"This recording can't be transcribed on this server. Add the lecture's captions instead."}
		}
		return s.transcribed(s.tr.TranscribeFile(ctx, file, m.geminiType))
	}
	segments, err := sources.FetchYoutubeSegments(ctx, nil, *src.VideoURL)
	if err == nil {
		return segments, nil
	}
	noCaptions := failure{"Could not fetch captions for this video (" + err.Error() + "). Upload its .vtt or .srt captions instead."}
	if !errors.Is(err, sources.ErrNoCaptions) || s.tr == nil {
		return nil, noCaptions
	}
	segments, err = s.tr.TranscribeURL(ctx, *src.VideoURL)
	if errors.Is(err, ai.ErrMedia) {
		return nil, failure{"This video has no captions and could not be transcribed (" + err.Error() + "). Upload its .vtt or .srt captions, or the recording itself, instead."}
	}
	return segments, err
}

// transcribed maps a recording the model can't use to a failure; anything else is retried.
func (s *svc) transcribed(segments []sources.TranscriptSegment, err error) ([]sources.TranscriptSegment, error) {
	if errors.Is(err, ai.ErrMedia) {
		return nil, failure{err.Error()}
	}
	return segments, err
}

// ingestPDF: a deck exported to PDF is still a deck, so with asSlides one page is one slide.
func (s *svc) ingestPDF(ctx context.Context, file []byte, asSlides bool) ([]store.NewChunk, error) {
	pages, err := sources.ExtractPDFPages(file)
	if err != nil {
		return nil, failure{"Could not read this PDF (" + err.Error() + ")."}
	}
	at := func(n int) (page, slide *int) {
		if asSlides {
			return nil, ptr(n)
		}
		return ptr(n), nil
	}
	var chunks []store.NewChunk
	for _, p := range pages {
		for _, text := range sources.SplitText(p.Text, sources.MaxChunkChars) {
			page, slide := at(p.Page)
			chunks = append(chunks, store.NewChunk{Modality: "text", Page: page, Slide: slide, Content: text})
		}
	}

	pageCount := 0
	if len(pages) > 0 {
		pageCount = pages[len(pages)-1].Page
	}
	if pageCount > 0 && pageCount <= maxPDFPagesForFigures {
		what := "document"
		if asSlides {
			what = "slide deck (one page per slide)"
		}
		figures := s.describeFigures(ctx,
			[]ai.Attachment{{PDF: true, MediaType: "application/pdf", Base64: base64.StdEncoding.EncodeToString(file)}},
			"Describe the figures in this "+what+". Give the page number each one is on.", ai.PDFFiguresSchema)
		for _, f := range figures {
			if f.Page == nil || *f.Page < 1 || *f.Page > pageCount {
				continue
			}
			page, slide := at(*f.Page)
			chunks = append(chunks, store.NewChunk{Modality: "figure", Page: page, Slide: slide, Content: "Figure: " + f.Description})
		}
	}
	return chunks, nil
}

func (s *svc) ingestPPTX(ctx context.Context, file []byte) ([]store.NewChunk, error) {
	slides, err := sources.ExtractPPTXSlides(file)
	if err != nil {
		return nil, failure{"Could not read this deck (" + err.Error() + ")."}
	}
	var chunks []store.NewChunk
	var images []ai.Attachment
	for _, sl := range slides {
		if sl.Text != "" {
			for _, text := range sources.SplitText(sl.Text, sources.MaxChunkChars) {
				chunks = append(chunks, store.NewChunk{Modality: "text", Slide: ptr(sl.Slide), Content: text})
			}
		}
		for i, img := range sl.Images {
			images = append(images, ai.Attachment{MediaType: img.MediaType, Base64: img.Base64,
				Label: fmt.Sprintf("Slide %d, picture %d:", sl.Slide, i+1)})
		}
	}
	for start := 0; start < len(images); start += imagesPerCall {
		figures := s.describeFigures(ctx, images[start:min(start+imagesPerCall, len(images))],
			"Describe the pictures above. Each is labelled with the slide it is on; give that slide number.", ai.SlideFiguresSchema)
		for _, f := range figures {
			if f.Slide == nil || *f.Slide < 1 {
				continue
			}
			chunks = append(chunks, store.NewChunk{Modality: "figure", Slide: ptr(*f.Slide), Content: "Figure: " + f.Description})
		}
	}
	return chunks, nil
}

type figure struct {
	Page        *int   `json:"page"`
	Slide       *int   `json:"slide"`
	Description string `json:"description"`
}

// describeFigures: figures are worth searching but not worth failing over. Without an API key,
// or when the model fails, the source keeps its text, which is most of what a learner needs.
func (s *svc) describeFigures(ctx context.Context, files []ai.Attachment, prompt string, schema any) []figure {
	if s.d.AI == nil {
		return nil
	}
	var out struct {
		Figures []figure `json:"figures"`
	}
	if _, err := s.d.AI.JSON(ctx, ai.Request{System: ai.FigureSystem, Prompt: prompt, Schema: schema, Attachments: files,
		Quality: ai.Standard, MaxTokens: 8000}, &out); err != nil {
		slog.Warn("figure description skipped", "err", err)
		return nil
	}
	var kept []figure
	for _, f := range out.Figures {
		if strings.TrimSpace(f.Description) != "" {
			kept = append(kept, f)
		}
	}
	return kept
}

// fileFormat reads magic bytes, not the file name: a renamed file is still what it is.
// A .pptx is a zip; whether it is really a deck is found out when it is opened.
func fileFormat(file []byte) string {
	switch {
	case bytes.HasPrefix(file, []byte("%PDF")):
		return "pdf"
	case bytes.HasPrefix(file, []byte("PK\x03\x04")):
		return "pptx"
	}
	return ""
}

// media is an accepted recording format: its file extension, the type it is stored and served
// with, and Gemini's name for it.
type media struct{ ext, contentType, geminiType string }

var mediaFormats = []media{
	{"mp3", "audio/mpeg", "audio/mp3"},
	{"m4a", "audio/mp4", "audio/mp4"},
	{"wav", "audio/wav", "audio/wav"},
	{"mp4", "video/mp4", "video/mp4"},
	{"webm", "video/webm", "video/webm"},
	{"mov", "video/quicktime", "video/mov"},
}

// mediaFormat reads magic bytes, as fileFormat does.
func mediaFormat(file []byte) (media, bool) {
	ext := ""
	box := ""
	if len(file) >= 12 {
		box = string(file[4:8])
	}
	switch {
	case bytes.HasPrefix(file, []byte("ID3")), len(file) >= 2 && file[0] == 0xFF && file[1]&0xE6 == 0xE2: // MPEG audio frame sync, layer III
		ext = "mp3"
	case bytes.HasPrefix(file, []byte("RIFF")) && len(file) >= 12 && string(file[8:12]) == "WAVE":
		ext = "wav"
	case bytes.HasPrefix(file, []byte("\x1A\x45\xDF\xA3")): // EBML: WebM (or Matroska, which Gemini also reads)
		ext = "webm"
	case box == "ftyp" && string(file[8:12]) == "qt  ", box == "moov", box == "mdat", box == "wide", box == "free":
		ext = "mov"
	case box == "ftyp" && strings.HasPrefix(string(file[8:12]), "M4A"):
		ext = "m4a"
	case box == "ftyp":
		ext = "mp4"
	}
	for _, m := range mediaFormats {
		if m.ext == ext {
			return m, true
		}
	}
	return media{}, false
}

// isMedia: a stored recording, by the extension store gave it.
func isMedia(path string) bool {
	for _, m := range mediaFormats {
		if strings.HasSuffix(path, "."+m.ext) {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }

// IngestNow adds a source and ingests it in this process, skipping the queue (d.Redis is not
// used). file is a PDF, or nil for a video, whose captions are fetched from YouTube. For
// cmd/evalgrounding -ingest. A problem with the material marks the source failed, as the
// worker would; the returned error is anything else.
func IngestNow(ctx context.Context, d *app.Deps, in store.NewSource, file []byte) error {
	s := newSvc(d)
	if file != nil {
		path := in.UserID + "/" + strings.ToLower(rand.Text()) + ".pdf"
		if err := s.files.upload(ctx, path, file, "application/pdf"); err != nil {
			return err
		}
		in.StoragePath = &path
	}
	src, err := s.sources().Create(ctx, in)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(job{SourceID: src.ID})
	if err := s.ingest(ctx, payload); err != nil {
		s.fail(ctx, src.ID, err.Error())
		return err
	}
	return nil
}

// Building the course map once the material is in. POST /sources/organize with whenReady
// leaves the course name here while any source is still processing; whichever ingestion job
// finishes the learner's last source builds the map. GETDEL makes exactly one job claim it.
func organizePendingKey(userID string) string { return "organize:pending:" + userID }
func organizeFailedKey(userID string) string  { return "organize:failed:" + userID }

const organizeKeyTTL = 24 * time.Hour

func (s *svc) organizeIfPending(ctx context.Context, userID string) {
	if s.d.Redis == nil { // in-process ingestion (IngestNow) has no queue and nothing pending
		return
	}
	ctx = context.WithoutCancel(ctx)
	if n, err := s.d.Redis.Exists(ctx, organizePendingKey(userID)).Result(); err != nil || n == 0 {
		return
	}
	processing, err := s.sources().HasProcessingSources(ctx, userID)
	if err != nil || processing {
		return
	}
	name, err := s.d.Redis.GetDel(ctx, organizePendingKey(userID)).Result()
	if err != nil {
		return // redis.Nil: another job claimed it
	}
	if _, err := s.organize(ctx, userID, name); err != nil {
		slog.Warn("organize after ingestion", "user", userID, "err", err)
		msg := "Could not build the course map. Try again from Sources."
		var he *httpx.Error
		if errors.As(err, &he) {
			msg = he.Message
		}
		s.d.Redis.Set(ctx, organizeFailedKey(userID), msg, organizeKeyTTL)
		return
	}
	s.d.Redis.Del(ctx, organizeFailedKey(userID))
}
