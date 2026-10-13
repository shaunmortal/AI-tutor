// Package content serves course material (/sources) and the tutor (/ai): uploads, the
// ingestion worker that turns them into located excerpts, the course map, and grounded answers.
package content

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/jobs"
	"github.com/shaunmortal/AI-tutor/server/internal/ratelimit"
	"github.com/shaunmortal/AI-tutor/server/internal/sources"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

const (
	// 40 MB holds a full textbook; Claude's own PDF limit is 32 MB, and a larger book still has
	// its text indexed, only its figures are skipped.
	maxUpload   = 40 << 20
	maxCaptions = 5 << 20
)

// The Redis stream ingestion jobs go through, and how long a job may run before another
// worker assumes it died. Vars so tests use their own.
var (
	ingestStream     = "jobs:ingest"
	ingestVisibility = 10 * time.Minute
)

type svc struct {
	d     *app.Deps
	files *storage
	queue *jobs.Queue
}

func newSvc(d *app.Deps) *svc {
	s := &svc{
		d:     d,
		files: &storage{url: d.Cfg.SupabaseURL, key: d.Cfg.SupabaseSecretKey, http: &http.Client{Timeout: 2 * time.Minute}},
		queue: jobs.New(d.Redis, ingestStream),
	}
	s.queue.Visibility = ingestVisibility
	s.queue.OnGiveUp = s.giveUp
	return s
}

func (s *svc) sources() *store.Sources { return store.NewSources(s.d.DB.Admin) }

// Register mounts /sources and /ai.
func Register(d *app.Deps) {
	for _, rt := range newSvc(d).routes() {
		d.Authed(rt.pattern, rt.h, rt.mw...)
	}
}

type route struct {
	pattern string
	h       httpx.Handler
	mw      []func(http.Handler) http.Handler
}

func (s *svc) routes() []route {
	rdb := s.d.Redis
	// Ingesting spends model calls on figures, so adding material is limited: 30 an hour covers
	// a whole course in one sitting. One quota across the three ways in, as in the TS.
	ingest := ratelimit.Limit(rdb, "sources-ingest", 30, time.Hour, "You have added a lot of material in the last hour.")
	organize := ratelimit.Limit(rdb, "sources-organize", 10, time.Hour, "You have rebuilt the course map a lot in the last hour.")
	// Every tutor call spends money: 40 an hour stops a loop in client code, not a learner.
	tutor := ratelimit.Limit(rdb, "ai-tutor", 40, time.Hour, "You have asked a lot of questions in the last hour.")
	type mw = func(http.Handler) http.Handler
	return []route{
		{"GET /sources", s.list, nil},
		{"POST /sources/upload", s.upload, []mw{ingest}},
		{"POST /sources/video", s.addVideo, []mw{ingest}},
		{"POST /sources/video/captions", s.addCaptions, []mw{ingest}},
		{"POST /sources/organize", s.organizeRoute, []mw{organize}},
		{"GET /sources/map/{curriculumId}", s.mapRoute, nil},
		{"GET /sources/excerpts/{id}/open", s.openRoute, nil},
		{"DELETE /sources/{id}", s.remove, nil},
		{"GET /ai/conversations", s.listConversations, nil},
		{"GET /ai/conversations/{id}", s.getConversation, nil},
		{"POST /ai/tutor", s.ask, []mw{tutor}},
		{"POST /ai/checks/{id}/answer", s.answerCheck, nil},
		{"GET /revision/flashcards", s.flashcards, nil},
		{"POST /revision/flashcards/{id}/review", s.reviewFlashcard, nil},
	}
}

func invalid(msg string) error { return httpx.E(http.StatusBadRequest, "INVALID_INPUT", msg) }

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func uuidParam(r *http.Request, name string) (string, error) {
	if v := r.PathValue(name); uuidRe.MatchString(v) {
		return v, nil
	}
	return "", invalid("That is not a valid id.")
}

// readBody reads a raw body of at most limit bytes; larger is a 413.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, httpx.E(http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "That file is too large.")
	}
	return b, err
}

// title is the zod rule: trimmed, 1 to 200 characters.
func title(raw string) (string, bool) {
	t := strings.TrimSpace(raw)
	n := len([]rune(t))
	return t, n >= 1 && n <= 200
}

// videoInput is addVideoSchema: the first failing rule's message, and its field.
func videoInput(rawURL, rawTitle string) (u, t, field, msg string) {
	u = strings.TrimSpace(rawURL)
	if p, err := url.Parse(u); err != nil || p.Scheme == "" || (p.Host == "" && p.Opaque == "") {
		return "", "", "url", "Invalid URL"
	}
	if sources.YoutubeID(u) == "" {
		return "", "", "url", "Use a YouTube link to the lecture."
	}
	t, ok := title(rawTitle)
	if !ok {
		if strings.TrimSpace(rawTitle) == "" {
			return "", "", "title", "Too small: expected string to have >=1 characters"
		}
		return "", "", "title", "Too big: expected string to have <=200 characters"
	}
	return u, t, "", ""
}

// --- /sources ---

type sourceRef struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func accepted(w http.ResponseWriter, src *store.SourceRow) error {
	httpx.OK(w, http.StatusAccepted, map[string]any{"source": sourceRef{src.ID, src.Status}})
	return nil
}

// POST /sources/upload?kind=pdf|slides&title=… with the raw file as the body.
func (s *svc) upload(w http.ResponseWriter, r *http.Request) error {
	userID := auth.UserID(r)
	q := r.URL.Query()
	kind := q.Get("kind")
	t, ok := title(q.Get("title"))
	if !ok || (kind != "pdf" && kind != "slides") {
		return invalid("Say what kind of file this is and give it a title.")
	}
	file, err := readBody(w, r, maxUpload)
	if err != nil {
		return err
	}
	if len(file) == 0 {
		return invalid("The upload was empty.")
	}
	format := fileFormat(file)
	if format == "" || (kind == "pdf" && format != "pdf") {
		msg := "Slides must be a .pptx or a PDF export of the deck."
		if kind == "pdf" {
			msg = "That file is not a PDF."
		}
		return httpx.E(http.StatusBadRequest, "UNSUPPORTED_FILE", msg)
	}
	contentType := "application/pdf"
	if format == "pptx" {
		contentType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	src, err := s.store(r.Context(), store.NewSource{UserID: userID, Kind: kind, Title: t}, format, file, contentType)
	if err != nil {
		return err
	}
	return accepted(w, src)
}

// POST /sources/video {url, title}: a YouTube lecture, transcribed from its published captions.
func (s *svc) addVideo(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		URL   *string `json:"url"`
		Title *string `json:"title"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		return err
	}
	if body.URL == nil {
		return invalid("url: Invalid input: expected string, received undefined")
	}
	if body.Title == nil {
		return invalid("title: Invalid input: expected string, received undefined")
	}
	u, t, field, msg := videoInput(*body.URL, *body.Title)
	if msg != "" {
		return invalid(field + ": " + msg)
	}
	src, err := s.sources().Create(r.Context(), store.NewSource{UserID: auth.UserID(r), Kind: "video", Title: t, VideoURL: &u})
	if err != nil {
		return err
	}
	if err := s.enqueue(r.Context(), src.ID); err != nil {
		return err
	}
	return accepted(w, src)
}

// POST /sources/video/captions?url=…&title=… with the .vtt/.srt as the body. The captions are
// kept in the bucket next to uploaded files, so the queued job needs nothing but the source id.
func (s *svc) addCaptions(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	u, t, _, msg := videoInput(q.Get("url"), q.Get("title"))
	if msg != "" {
		return invalid(msg)
	}
	captions, err := readBody(w, r, maxCaptions)
	if err != nil {
		return err
	}
	if !strings.Contains(string(captions), "-->") {
		return invalid("That does not look like a .vtt or .srt file.")
	}
	src, err := s.store(r.Context(), store.NewSource{UserID: auth.UserID(r), Kind: "video", Title: t, VideoURL: &u},
		"vtt", captions, "text/vtt")
	if err != nil {
		return err
	}
	return accepted(w, src)
}

// store puts the file in the bucket, creates the source row ('processing') and queues it.
func (s *svc) store(ctx context.Context, in store.NewSource, ext string, file []byte, contentType string) (*store.SourceRow, error) {
	// The user id leads the path, so one learner's files can never be addressed by another's.
	path := in.UserID + "/" + strings.ToLower(rand.Text()) + "." + ext
	if err := s.files.upload(ctx, path, file, contentType); err != nil {
		return nil, err
	}
	in.StoragePath = &path
	src, err := s.sources().Create(ctx, in)
	if err != nil {
		_ = s.files.remove(context.WithoutCancel(ctx), path)
		return nil, err
	}
	return src, s.enqueue(ctx, src.ID)
}

// enqueue queues ingestion. If Redis is down the source is marked failed rather than left
// 'processing' with no job behind it.
func (s *svc) enqueue(ctx context.Context, sourceID string) error {
	if err := s.queue.Enqueue(ctx, job{SourceID: sourceID}); err != nil {
		slog.Error("enqueue ingest", "source", sourceID, "err", err)
		msg := "Could not start processing. Delete this source and add it again."
		_ = s.sources().SetStatus(context.WithoutCancel(ctx), sourceID, "failed", &msg)
		return err
	}
	return nil
}

type sourceView struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	Error        *string   `json:"error"`
	ExcerptCount int       `json:"excerptCount"`
	CurriculumID *string   `json:"curriculumId"`
	CreatedAt    time.Time `json:"createdAt"`
}

// GET /sources
func (s *svc) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.sources().List(r.Context(), auth.UserID(r))
	if err != nil {
		return err
	}
	out := make([]sourceView, len(rows))
	for i, x := range rows {
		out[i] = sourceView{x.ID, x.Kind, x.Title, x.Status, x.Error, x.ChunkCount, x.CurriculumID, x.CreatedAt}
	}
	// Whether a course map is waiting on this material (whenReady), or failed to build after it.
	organize := map[string]any{"status": "idle"}
	userID := auth.UserID(r)
	if n, _ := s.d.Redis.Exists(r.Context(), organizePendingKey(userID)).Result(); n > 0 {
		organize["status"] = "waiting"
	} else if msg, err := s.d.Redis.Get(r.Context(), organizeFailedKey(userID)).Result(); err == nil {
		organize = map[string]any{"status": "failed", "message": msg}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"sources": out, "organize": organize})
	return nil
}

// DELETE /sources/{id}
func (s *svc) remove(w http.ResponseWriter, r *http.Request) error {
	id, err := uuidParam(r, "id")
	if err != nil {
		return err
	}
	src, err := s.sources().Remove(r.Context(), id, auth.UserID(r))
	if err != nil {
		return err
	}
	if src.StoragePath != nil {
		// As in the TS, a file that will not delete does not undo deleting the source.
		if err := s.files.remove(r.Context(), *src.StoragePath); err != nil {
			slog.Warn("remove source file", "path", *src.StoragePath, "err", err)
		}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"deleted": true})
	return nil
}

// POST /sources/organize {courseName}: build the course map from everything uploaded so far.
func (s *svc) organizeRoute(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		CourseName *string `json:"courseName"`
		// WhenReady: if material is still processing, build the map when it finishes (202).
		WhenReady bool `json:"whenReady"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		return err
	}
	if body.CourseName == nil {
		return invalid("courseName: Invalid input: expected string, received undefined")
	}
	name := strings.TrimSpace(*body.CourseName)
	switch n := len([]rune(name)); {
	case n < 2:
		return invalid("courseName: Too small: expected string to have >=2 characters")
	case n > 120:
		return invalid("courseName: Too big: expected string to have <=120 characters")
	}
	userID, ctx := auth.UserID(r), r.Context()
	if body.WhenReady {
		processing, err := s.sources().HasProcessingSources(ctx, userID)
		if err != nil {
			return err
		}
		if processing {
			s.d.Redis.Del(ctx, organizeFailedKey(userID))
			if err := s.d.Redis.Set(ctx, organizePendingKey(userID), name, organizeKeyTTL).Err(); err != nil {
				return err
			}
			// The last source may have finished between the check and the write.
			go s.organizeIfPending(ctx, userID)
			httpx.OK(w, http.StatusAccepted, map[string]any{"status": "waiting"})
			return nil
		}
	}
	m, err := s.organize(ctx, userID, name)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, m)
	return nil
}

// GET /sources/map/{curriculumId}
func (s *svc) mapRoute(w http.ResponseWriter, r *http.Request) error {
	id, err := uuidParam(r, "curriculumId")
	if err != nil {
		return err
	}
	m, err := s.courseMap(r.Context(), id, auth.UserID(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, m)
	return nil
}

// openedExcerpt is where a citation chip goes. Slide is new in the Go port: a .pptx has no
// viewer that can jump to a slide, so the link opens the deck and the client says "slide N".
type openedExcerpt struct {
	URL         *string `json:"url"`
	Slide       *int    `json:"slide"`
	SourceTitle string  `json:"sourceTitle"`
	Location    string  `json:"location"`
	Excerpt     string  `json:"excerpt"`
}

// GET /sources/excerpts/{id}/open
func (s *svc) openRoute(w http.ResponseWriter, r *http.Request) error {
	id, err := uuidParam(r, "id")
	if err != nil {
		return err
	}
	o, err := s.openExcerpt(r.Context(), id, auth.UserID(r))
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, o)
	return nil
}

func (s *svc) openExcerpt(ctx context.Context, chunkID, userID string) (openedExcerpt, error) {
	repo := s.sources()
	chunk, err := repo.FindChunk(ctx, chunkID, userID)
	if err != nil {
		return openedExcerpt{}, err
	}
	src, err := repo.Find(ctx, chunk.SourceID, userID)
	if err != nil {
		return openedExcerpt{}, err
	}
	out := openedExcerpt{Slide: chunk.Slide, SourceTitle: src.Title, Location: locationOf(*chunk), Excerpt: chunk.Content}
	switch {
	case src.VideoURL != nil && chunk.StartSec != nil:
		u := sources.YoutubeAt(*src.VideoURL, *chunk.StartSec)
		out.URL = &u
	case src.StoragePath != nil && (strings.HasSuffix(*src.StoragePath, ".pdf") || strings.HasSuffix(*src.StoragePath, ".pptx")):
		u, err := s.files.signedURL(ctx, *src.StoragePath)
		if err != nil {
			return openedExcerpt{}, err
		}
		page := chunk.Page
		if page == nil {
			page = chunk.Slide
		}
		if page != nil && strings.HasSuffix(*src.StoragePath, ".pdf") {
			u += "#page=" + strconv.Itoa(*page)
		}
		out.URL = &u
	}
	return out, nil
}

// --- /ai ---

// GET /ai/conversations. Reads run as the learner, so RLS is a second lock behind the user id.
func (s *svc) listConversations(w http.ResponseWriter, r *http.Request) error {
	userID := auth.UserID(r)
	var out []ai.ConversationSummary
	err := s.d.DB.AsUser(r.Context(), userID, func(tx pgx.Tx) (err error) {
		out, err = (&ai.Tutor{Conversations: conversations{store.NewAI(tx)}}).ListConversations(r.Context(), userID)
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"conversations": out})
	return nil
}

// GET /ai/conversations/{id}
func (s *svc) getConversation(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		return invalid("That is not a conversation id.")
	}
	userID := auth.UserID(r)
	var out ai.ConversationView
	err := s.d.DB.AsUser(r.Context(), userID, func(tx pgx.Tx) (err error) {
		out, err = (&ai.Tutor{Conversations: conversations{store.NewAI(tx)}}).GetConversation(r.Context(), id, userID)
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, out)
	return nil
}

// POST /ai/tutor. Elevated: writing a conversation and reading the mastery a reply is grounded
// in are closed to learners. Every query is filtered by the user id from the token.
func (s *svc) ask(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Question        *string `json:"question"`
		ConversationID  *string `json:"conversationId"`
		TopicID         *string `json:"topicId"`
		CurrentQuestion *string `json:"currentQuestion"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		return err
	}
	if body.Question == nil {
		return invalid("question: Invalid input: expected string, received undefined")
	}
	question := strings.TrimSpace(*body.Question)
	switch n := len([]rune(question)); {
	case n < 2:
		return invalid("question: Too small: expected string to have >=2 characters")
	case n > ai.MaxQuestion:
		return invalid("question: Too big: expected string to have <=2000 characters")
	}
	for _, f := range []struct {
		name string
		v    *string
	}{{"conversationId", body.ConversationID}, {"topicId", body.TopicID}} {
		if f.v != nil && !uuidRe.MatchString(*f.v) {
			return invalid(f.name + ": Invalid UUID")
		}
	}
	if body.CurrentQuestion != nil && len([]rune(*body.CurrentQuestion)) > 2000 {
		return invalid("currentQuestion: Too big: expected string to have <=2000 characters")
	}

	in := ai.AskInput{UserID: auth.UserID(r), Question: question, TopicID: body.TopicID, CurrentQuestion: body.CurrentQuestion}
	if body.ConversationID != nil {
		in.ConversationID = *body.ConversationID
	}
	t := &ai.Tutor{AI: s.d.AI, Conversations: conversations{store.NewAI(s.d.DB.Admin)}, Material: material{s.sources(), s.d.Embed}}
	reply, err := t.Ask(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, reply)
	return nil
}
