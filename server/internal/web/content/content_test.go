package content

// Handler and worker tests against the live database (DATABASE_URL), Supabase Storage
// (SUPABASE_URL + SUPABASE_SECRET_KEY) and a local Redis, all read from the env or server/.env.
// Skipped when any is missing. Users are throwaway auth.users rows deleted at the end, along
// with every file they uploaded; the cleanup checks nothing is left. AI is nil throughout.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/config"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	s      *svc
	mux    *http.ServeMux
	u1, u2 string
}

func getenv(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	raw, _ := os.ReadFile("../../../.env")
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func setup(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{SupabaseURL: strings.TrimRight(getenv("SUPABASE_URL"), "/"), SupabaseSecretKey: getenv("SUPABASE_SECRET_KEY"), DatabaseURL: getenv("DATABASE_URL")}
	if cfg.DatabaseURL == "" || cfg.SupabaseURL == "" || cfg.SupabaseSecretKey == "" {
		t.Skip("DATABASE_URL / SUPABASE_URL / SUPABASE_SECRET_KEY not set")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if rdb.Ping(ctx).Err() != nil {
		t.Skip("redis not running")
	}
	database, err := db.Open(ctx, cfg.DatabaseURL, false)
	must(t, err)

	ingestStream = fmt.Sprintf("test-ingest-%s-%d", t.Name(), time.Now().UnixNano())
	ingestVisibility = 300 * time.Millisecond
	d := &app.Deps{Cfg: cfg, DB: database, Redis: rdb}
	e := &env{t: t, ctx: ctx, s: newSvc(d), mux: http.NewServeMux()}
	e.u1, e.u2 = e.newUser(), e.newUser()

	// The real route table, with auth replaced by the X-Test-User header.
	for _, rt := range e.s.routes() {
		var h http.Handler = httpx.Wrap(rt.h)
		for i := len(rt.mw) - 1; i >= 0; i-- {
			h = rt.mw[i](h)
		}
		method, path, _ := strings.Cut(rt.pattern, " ")
		e.mux.Handle(method+" /api/v1"+path, withUser(h))
	}

	t.Cleanup(func() {
		users := []string{e.u1, e.u2}
		paths, _ := e.col(`select storage_path from course_sources where user_id = any($1::uuid[]) and storage_path is not null`, users)
		for _, p := range paths {
			must(t, e.s.files.remove(ctx, p))
		}
		_, err := database.Admin.Exec(ctx, `delete from auth.users where id = any($1::uuid[])`, users)
		must(t, err)
		left, _ := e.col(`select id::text from course_sources where user_id = any($1::uuid[])
			union all select id::text from ai_conversations where user_id = any($1::uuid[])`, users)
		if len(left) > 0 {
			t.Errorf("left behind: %v", left)
		}
		for _, p := range paths {
			if _, err := e.s.files.download(ctx, p); err == nil {
				t.Errorf("storage object %s left behind", p)
			}
		}
		for _, u := range users {
			rdb.Del(ctx, "rl:sources-ingest:"+u, "rl:sources-organize:"+u, "rl:ai-tutor:"+u)
		}
		rdb.Del(ctx, ingestStream)
		database.Close()
		rdb.Close()
	})
	return e
}

func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), r.Header.Get("X-Test-User"))))
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) col(sql string, args ...any) ([]string, error) {
	rows, err := e.s.d.DB.Admin.Query(e.ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (e *env) newUser() string {
	var id string
	must(e.t, e.s.d.DB.Admin.QueryRow(e.ctx, `with id as (select gen_random_uuid() as id)
		insert into auth.users (id, email) select id, id || '@adigam.test' from id returning id::text`).Scan(&id))
	return id
}

type reply struct {
	status int
	Data   json.RawMessage
	Error  *struct{ Code, Message string }
}

func (e *env) do(user, method, path, contentType string, body []byte) reply {
	e.t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(body))
	req.Header.Set("X-Test-User", user)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	var r reply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		e.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	r.status = w.Code
	return r
}

func (e *env) json(user, method, path string, body any) reply {
	b, _ := json.Marshal(body)
	return e.do(user, method, path, "application/json", b)
}

func (r reply) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Data, v); err != nil {
		t.Fatalf("decode %s: %v", r.Data, err)
	}
}

func expectError(t *testing.T, r reply, status int, code, msg string) {
	t.Helper()
	if r.status != status || r.Error == nil || r.Error.Code != code || (msg != "" && r.Error.Message != msg) {
		t.Errorf("want %d %s %q, got %d %+v", status, code, msg, r.status, r.Error)
	}
}

// runWorker consumes the test stream until the test ends.
func (e *env) runWorker() {
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.s.queue.Run(ctx, e.s.ingest); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

// waitFor polls the source list until the source leaves 'processing'.
func (e *env) waitFor(user, id string) sourceView {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var out struct{ Sources []sourceView }
		e.do(user, "GET", "/sources", "", nil).into(e.t, &out)
		for _, s := range out.Sources {
			if s.ID == id && s.Status != "processing" {
				return s
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("source %s still processing", id)
	return sourceView{}
}

func (e *env) firstChunk(sourceID string) string {
	ids, err := e.col(`select id::text from source_chunks where source_id = $1 order by page nulls last, slide nulls last, start_sec nulls last`, sourceID)
	must(e.t, err)
	if len(ids) == 0 {
		e.t.Fatal("no chunks")
	}
	return ids[0]
}

func (e *env) add(user, path, contentType string, body []byte) string {
	e.t.Helper()
	r := e.do(user, "POST", path, contentType, body)
	var out struct{ Source sourceRef }
	r.into(e.t, &out)
	if r.status != 202 || out.Source.Status != "processing" {
		e.t.Fatalf("add: %d %s %+v", r.status, r.Data, r.Error)
	}
	return out.Source.ID
}

func tinyPDF() []byte {
	content := "BT /F1 12 Tf 20 250 Td (Normalisation removes redundancy from relational tables.) Tj ET"
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [4 0 R] /Count 1 >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>")
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

func tinyPPTX() []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ppt/slides/slide1.xml")
	io.WriteString(w, `<p:sld><a:p><a:r><a:t xml:space="preserve">A heap is a complete binary tree.</a:t></a:r></a:p></p:sld>`)
	zw.Close()
	return buf.Bytes()
}

const vtt = "WEBVTT\n\n00:00:01.000 --> 00:00:04.000\nToday we study heaps.\n\n00:00:05.000 --> 00:00:08.000\nA heap is a tree.\n"

func TestIngestThroughQueueAndOpenExcerpts(t *testing.T) {
	e := setup(t)
	e.runWorker()

	// PDF: page link.
	pdf := e.add(e.u1, "/sources/upload?kind=pdf&title=Notes", "application/pdf", tinyPDF())
	if got := e.waitFor(e.u1, pdf); got.Status != "ready" || got.ExcerptCount != 1 || got.Kind != "pdf" || got.Title != "Notes" {
		t.Fatalf("pdf: %+v", got)
	}
	var opened openedExcerpt
	r := e.do(e.u1, "GET", "/sources/excerpts/"+e.firstChunk(pdf)+"/open", "", nil)
	r.into(t, &opened)
	if r.status != 200 || opened.URL == nil || !strings.Contains(*opened.URL, "/storage/v1/object/sign/course-sources/") ||
		!strings.HasSuffix(*opened.URL, "#page=1") || opened.Location != "p. 1" || opened.Slide != nil {
		t.Fatalf("open pdf: %d %s", r.status, r.Data)
	}
	if res, err := http.Get(strings.TrimSuffix(*opened.URL, "#page=1")); err != nil || res.StatusCode != 200 {
		t.Fatalf("signed url does not open: %v %v", err, res)
	}

	// PPTX (regression: used to open nothing): signed link to the deck plus the slide number.
	deck := e.add(e.u1, "/sources/upload?kind=slides&title=Deck", "application/octet-stream", tinyPPTX())
	if got := e.waitFor(e.u1, deck); got.Status != "ready" || got.ExcerptCount != 1 {
		t.Fatalf("pptx: %+v", got)
	}
	opened = openedExcerpt{}
	e.do(e.u1, "GET", "/sources/excerpts/"+e.firstChunk(deck)+"/open", "", nil).into(t, &opened)
	if opened.URL == nil || strings.Contains(*opened.URL, "#page") || opened.Slide == nil || *opened.Slide != 1 ||
		opened.Location != "slide 1" || opened.Excerpt != "A heap is a complete binary tree." {
		t.Fatalf("open pptx: %+v", opened)
	}

	// Captions: the video at that second.
	video := e.add(e.u1, "/sources/video/captions?url=https://youtu.be/dQw4w9WgXcQ&title=Lecture", "text/vtt", []byte(vtt))
	if got := e.waitFor(e.u1, video); got.Status != "ready" || got.ExcerptCount != 1 || got.Kind != "video" {
		t.Fatalf("captions: %+v", got)
	}
	opened = openedExcerpt{}
	e.do(e.u1, "GET", "/sources/excerpts/"+e.firstChunk(video)+"/open", "", nil).into(t, &opened)
	if opened.URL == nil || *opened.URL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=1s" || opened.Location != "0:01" {
		t.Fatalf("open video: %+v", opened)
	}

	// Someone else's excerpt and source are 404s.
	expectError(t, e.do(e.u2, "GET", "/sources/excerpts/"+e.firstChunk(pdf)+"/open", "", nil), 404, "EXCERPT_NOT_FOUND", "")
	expectError(t, e.do(e.u2, "DELETE", "/sources/"+pdf, "", nil), 404, "SOURCE_NOT_FOUND", "")

	// Idempotent: running the job again for the same source replaces, never duplicates.
	must(t, e.s.sources().SetStatus(e.ctx, pdf, "processing", nil))
	payload, _ := json.Marshal(job{SourceID: pdf})
	must(t, e.s.ingest(e.ctx, payload))
	must(t, e.s.ingest(e.ctx, payload))
	if n, _ := e.col(`select id::text from source_chunks where source_id = $1`, pdf); len(n) != 1 {
		t.Fatalf("re-ingest left %d chunks", len(n))
	}

	// Delete removes the row and the file.
	path, _ := e.col(`select storage_path from course_sources where id = $1`, pdf)
	r = e.do(e.u1, "DELETE", "/sources/"+pdf, "", nil)
	if r.status != 200 || string(r.Data) != `{"deleted":true}` {
		t.Fatalf("delete: %d %s", r.status, r.Data)
	}
	if _, err := e.s.files.download(e.ctx, path[0]); err == nil {
		t.Fatal("file still in the bucket")
	}
}

func TestIngestFailures(t *testing.T) {
	e := setup(t)
	e.runWorker()

	// Material problem: failed at once with a readable message.
	bad := e.add(e.u1, "/sources/upload?kind=slides&title=Broken", "application/octet-stream", []byte("PK\x03\x04not a zip"))
	if got := e.waitFor(e.u1, bad); got.Status != "failed" || got.Error == nil || !strings.HasPrefix(*got.Error, "Could not read this deck") {
		t.Fatalf("broken deck: %+v", got)
	}

	// Infrastructure problem (the file is gone): retried, then given up on with a message.
	path := e.u1 + "/missing.pdf"
	src, err := e.s.sources().Create(e.ctx, store.NewSource{UserID: e.u1, Kind: "pdf", Title: "Lost", StoragePath: &path})
	must(t, err)
	must(t, e.s.enqueue(e.ctx, src.ID))
	if got := e.waitFor(e.u1, src.ID); got.Status != "failed" || got.Error == nil || !strings.HasPrefix(*got.Error, "Processing failed after several attempts") {
		t.Fatalf("given up: %+v", got)
	}
}

func TestRecoverOrphans(t *testing.T) {
	e := setup(t) // no worker: only checks what gets queued
	path := e.u1 + "/orphan.pdf"
	src, err := e.s.sources().Create(e.ctx, store.NewSource{UserID: e.u1, Kind: "pdf", Title: "Orphan", StoragePath: &path})
	must(t, err)
	_, err = e.s.d.DB.Admin.Exec(e.ctx, `update course_sources set created_at = now() - interval '5 minutes' where id = $1`, src.ID)
	must(t, err)

	count := func() int {
		msgs, err := e.s.d.Redis.XRange(e.ctx, ingestStream, "-", "+").Result()
		must(t, err)
		n := 0
		for _, m := range msgs {
			if strings.Contains(fmt.Sprint(m.Values["p"]), src.ID) {
				n++
			}
		}
		return n
	}
	e.s.recoverOrphans(e.ctx)
	e.s.recoverOrphans(e.ctx) // already queued: not queued twice
	if n := count(); n != 1 {
		t.Fatalf("queued %d times", n)
	}
}

func TestSourceValidation(t *testing.T) {
	e := setup(t)
	u := e.u1
	expectError(t, e.do(u, "POST", "/sources/upload?kind=doc&title=x", "", tinyPDF()), 400, "INVALID_INPUT", "Say what kind of file this is and give it a title.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=pdf&title=%20", "", tinyPDF()), 400, "INVALID_INPUT", "Say what kind of file this is and give it a title.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=pdf&title=x", "", nil), 400, "INVALID_INPUT", "The upload was empty.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=pdf&title=x", "", tinyPPTX()), 400, "UNSUPPORTED_FILE", "That file is not a PDF.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=slides&title=x", "", []byte("hello")), 400, "UNSUPPORTED_FILE", "Slides must be a .pptx or a PDF export of the deck.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=pdf&title=x", "", make([]byte, maxUpload+1)), 413, "PAYLOAD_TOO_LARGE", "")
	expectError(t, e.json(u, "POST", "/sources/video", map[string]string{"url": "https://vimeo.com/1", "title": "x"}), 400, "INVALID_INPUT", "url: Use a YouTube link to the lecture.")
	expectError(t, e.json(u, "POST", "/sources/video", map[string]string{"url": "nope", "title": "x"}), 400, "INVALID_INPUT", "url: Invalid URL")
	expectError(t, e.json(u, "POST", "/sources/video", map[string]string{"url": "https://youtu.be/dQw4w9WgXcQ"}), 400, "INVALID_INPUT", "title: Invalid input: expected string, received undefined")
	expectError(t, e.do(u, "POST", "/sources/video/captions?url=https://vimeo.com/1&title=x", "", []byte(vtt)), 400, "INVALID_INPUT", "Use a YouTube link to the lecture.")
	expectError(t, e.do(u, "POST", "/sources/video/captions?url=https://youtu.be/dQw4w9WgXcQ&title=x", "", []byte("hello")), 400, "INVALID_INPUT", "That does not look like a .vtt or .srt file.")
	expectError(t, e.do(u, "DELETE", "/sources/not-a-uuid", "", nil), 400, "INVALID_INPUT", "That is not a valid id.")
	expectError(t, e.do(u, "GET", "/sources/map/nope", "", nil), 400, "INVALID_INPUT", "That is not a valid id.")
	expectError(t, e.json(u, "POST", "/sources/organize", map[string]string{"courseName": "x"}), 400, "INVALID_INPUT", "courseName: Too small: expected string to have >=2 characters")
	expectError(t, e.json(u, "POST", "/sources/organize", map[string]string{"courseName": "Databases"}), 409, "NO_MATERIAL", "")

	// With material but no API key: 503 AI_UNAVAILABLE, and nothing written.
	path := u + "/x.pdf"
	src, err := e.s.sources().Create(e.ctx, store.NewSource{UserID: u, Kind: "pdf", Title: "Notes", StoragePath: &path})
	must(t, err)
	must(t, e.s.sources().InsertChunks(e.ctx, *src, []store.NewChunk{{Modality: "text", Page: ptr(1), Content: "Keys."}}))
	expectError(t, e.json(u, "POST", "/sources/organize", map[string]string{"courseName": "Databases"}), 503, "AI_UNAVAILABLE", "")

	// The course map of the shipped curriculum, read by any learner.
	var curriculumID string
	must(t, e.s.d.DB.Admin.QueryRow(e.ctx, `select id::text from curricula where slug = $1`, store.DefaultCurriculumSlug).Scan(&curriculumID))
	var m courseMapView
	r := e.do(u, "GET", "/sources/map/"+curriculumID, "", nil)
	r.into(t, &m)
	if r.status != 200 || m.Curriculum == nil || m.Curriculum.ID != curriculumID || len(m.Subjects) == 0 || m.Subjects[0].Topics == nil {
		t.Fatalf("course map: %d %.300s", r.status, r.Data)
	}
}

func TestRateLimitIsSharedAcrossIngestRoutes(t *testing.T) {
	e := setup(t)
	must(t, e.s.d.Redis.Set(e.ctx, "rl:sources-ingest:"+e.u1, 30, time.Hour).Err())
	expectError(t, e.json(e.u1, "POST", "/sources/video", map[string]string{"url": "https://youtu.be/dQw4w9WgXcQ", "title": "x"}), 429, "RATE_LIMITED", "")
}

func TestTutorRoutes(t *testing.T) {
	e := setup(t)
	u := e.u1
	expectError(t, e.json(u, "POST", "/ai/tutor", map[string]string{"question": " a "}), 400, "INVALID_INPUT", "question: Too small: expected string to have >=2 characters")
	expectError(t, e.json(u, "POST", "/ai/tutor", map[string]string{"question": "why?", "conversationId": "x"}), 400, "INVALID_INPUT", "conversationId: Invalid UUID")
	expectError(t, e.do(u, "GET", "/ai/conversations/nope", "", nil), 400, "INVALID_INPUT", "That is not a conversation id.")

	// No API key: the question is kept, the reply is a 503, and the thread shows as unanswered.
	expectError(t, e.json(u, "POST", "/ai/tutor", map[string]string{"question": "What is a heap?"}), 503, "AI_UNAVAILABLE", "")
	var list struct {
		Conversations []struct {
			ID, Title string
			Answered  bool
		}
	}
	e.do(u, "GET", "/ai/conversations", "", nil).into(t, &list)
	if len(list.Conversations) != 1 || list.Conversations[0].Title != "What is a heap?" || list.Conversations[0].Answered {
		t.Fatalf("list: %+v", list)
	}
	id := list.Conversations[0].ID
	var view struct {
		Messages []struct{ Role, Content string }
	}
	r := e.do(u, "GET", "/ai/conversations/"+id, "", nil)
	r.into(t, &view)
	if r.status != 200 || len(view.Messages) != 1 || view.Messages[0].Role != "learner" {
		t.Fatalf("conversation: %s", r.Data)
	}
	expectError(t, e.do(e.u2, "GET", "/ai/conversations/"+id, "", nil), 404, "CONVERSATION_NOT_FOUND", "")

	// Regression: with more than 50 messages, history is the latest turns in order, not the
	// thread's opening.
	conv := conversations{store.NewAI(e.s.d.DB.Admin)}
	for i := 2; i <= 60; i++ {
		_, err := conv.AddMessage(e.ctx, aiMessage(id, i))
		must(t, err)
	}
	msgs, err := conv.ListMessages(e.ctx, id, 8)
	must(t, err)
	if len(msgs) != 8 || msgs[0].Content != "m53" || msgs[7].Content != "m60" {
		t.Fatalf("history: first %q last %q of %d", msgs[0].Content, msgs[len(msgs)-1].Content, len(msgs))
	}
}

func aiMessage(conversationID string, i int) ai.NewMessage {
	return ai.NewMessage{ConversationID: conversationID, Role: "learner", Content: fmt.Sprintf("m%d", i)}
}

func TestMediaFormat(t *testing.T) {
	box := box4
	for _, c := range []struct {
		file []byte
		ext  string
	}{
		{[]byte("ID3\x04\x00rest"), "mp3"},
		{[]byte("\xFF\xFB\x90\x00"), "mp3"},
		{[]byte("\xFF\xF1\x50\x80"), ""}, // ADTS AAC, not MPEG layer III
		{[]byte("RIFF\x24\x00\x00\x00WAVEfmt "), "wav"},
		{[]byte("RIFF\x24\x00\x00\x00AVI LIST"), ""},
		{[]byte("\x1A\x45\xDF\xA3\x01\x00"), "webm"},
		{box("ftyp", "M4A "), "m4a"},
		{box("ftyp", "isom"), "mp4"},
		{box("ftyp", "qt  "), "mov"},
		{box("wide", "\x00\x00\x00\x08"), "mov"},
		{[]byte("%PDF-1.4"), ""},
		{[]byte("PK\x03\x04"), ""},
		{nil, ""},
	} {
		m, ok := mediaFormat(c.file)
		if m.ext != c.ext || ok != (c.ext != "") {
			t.Errorf("mediaFormat(%q) = %q %v, want %q", c.file, m.ext, ok, c.ext)
		}
	}
	if !isMedia("u/x.m4a") || isMedia("u/x.vtt") || isMedia("u/x.pdf") {
		t.Error("isMedia")
	}
}

func TestUploadMediaRules(t *testing.T) {
	e := setup(t)
	u := e.u1
	// No Gemini key: refused up front rather than failing in the job.
	expectError(t, e.do(u, "POST", "/sources/upload?kind=media&title=x", "", box4("ftyp", "M4A ")), 503, "TRANSCRIPTION_UNAVAILABLE", "")
	e.s.tr = ai.NewTranscriber("unused")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=media&title=x", "", tinyPDF()), 400, "UNSUPPORTED_FILE", "Recordings must be .mp3, .m4a, .wav, .mp4, .webm or .mov.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=media&title=x", "", nil), 400, "INVALID_INPUT", "The upload was empty.")
	expectError(t, e.do(u, "POST", "/sources/upload?kind=media&title=x", "", make([]byte, maxMedia+1)), 413, "PAYLOAD_TOO_LARGE", "")
}

func box4(typ, brand string) []byte {
	return []byte("\x00\x00\x00\x1c" + typ + brand + "\x00\x00\x00\x00")
}

// The whole path with the real Gemini API: upload a spoken m4a, the job transcribes it, the
// excerpt opens the stored recording at its second. GEMINI_LIVE=1 and macOS say/afconvert.
func TestUploadedRecordingLive(t *testing.T) {
	if os.Getenv("GEMINI_LIVE") == "" {
		t.Skip("set GEMINI_LIVE=1 to call the real API")
	}
	e := setup(t)
	e.s.tr = ai.NewTranscriber(getenv("GEMINI_API_KEY"))
	e.runWorker()
	dir := t.TempDir()
	if out, err := exec.Command("say", "-o", dir+"/x.aiff", "binary search halves the interval").CombinedOutput(); err != nil {
		t.Skipf("say: %v %s", err, out)
	}
	if out, err := exec.Command("afconvert", "-f", "m4af", "-d", "aac", dir+"/x.aiff", dir+"/x.m4a").CombinedOutput(); err != nil {
		t.Fatalf("afconvert: %v %s", err, out)
	}
	audio, err := os.ReadFile(dir + "/x.m4a")
	must(t, err)
	id := e.add(e.u1, "/sources/upload?kind=media&title=Lecture", "audio/mp4", audio)
	if got := e.waitFor(e.u1, id); got.Status != "ready" || got.Kind != "video" || got.ExcerptCount != 1 {
		t.Fatalf("recording: %+v", got)
	}
	var opened openedExcerpt
	e.do(e.u1, "GET", "/sources/excerpts/"+e.firstChunk(id)+"/open", "", nil).into(t, &opened)
	if opened.URL == nil || !strings.Contains(*opened.URL, ".m4a") || !strings.HasSuffix(*opened.URL, "#t=0") ||
		opened.AtSec == nil || *opened.AtSec != 0 || !strings.Contains(strings.ToLower(opened.Excerpt), "binary search") {
		t.Fatalf("open recording: %+v", opened)
	}
	t.Logf("excerpt %q at %s", opened.Excerpt, opened.Location)
}
