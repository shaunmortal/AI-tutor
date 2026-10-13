package store

import (
	"context"
	"net/http"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// SourceRow is one uploaded course source.
type SourceRow struct {
	ID           string    `db:"id" json:"id"`
	UserID       string    `db:"user_id" json:"user_id"`
	CurriculumID *string   `db:"curriculum_id" json:"curriculum_id"`
	Kind         string    `db:"kind" json:"kind"` // pdf | slides | video
	Title        string    `db:"title" json:"title"`
	StoragePath  *string   `db:"storage_path" json:"storage_path"`
	VideoURL     *string   `db:"video_url" json:"video_url"`
	Status       string    `db:"status" json:"status"` // processing | ready | failed
	Error        *string   `db:"error" json:"error"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// SourceWithCount is a source with how many excerpts it has.
type SourceWithCount struct {
	SourceRow
	ChunkCount int `db:"chunk_count" json:"chunkCount"`
}

// ChunkRow is one excerpt. Rank is set only by Search.
type ChunkRow struct {
	ID       string   `db:"id" json:"id"`
	SourceID string   `db:"source_id" json:"source_id"`
	TopicID  *string  `db:"topic_id" json:"topic_id"`
	Modality string   `db:"modality" json:"modality"` // text | figure | transcript
	Page     *int     `db:"page" json:"page"`
	Slide    *int     `db:"slide" json:"slide"`
	StartSec *int     `db:"start_sec" json:"start_sec"`
	EndSec   *int     `db:"end_sec" json:"end_sec"`
	Content  string   `db:"content" json:"content"`
	Rank     *float64 `db:"-" json:"rank,omitempty"`
}

// NewSource is a source to create.
type NewSource struct {
	UserID      string
	Kind        string
	Title       string
	StoragePath *string
	VideoURL    *string
}

// NewChunk is an excerpt to store.
type NewChunk struct {
	Modality string
	Page     *int
	Slide    *int
	StartSec *int
	EndSec   *int
	Content  string
}

// ChunkAssignment tags one excerpt with a topic.
type ChunkAssignment struct {
	ChunkID string
	TopicID string
}

// Prerequisite says TopicID needs PrerequisiteID first.
type Prerequisite struct {
	TopicID        string `db:"topic_id" json:"topic_id"`
	PrerequisiteID string `db:"prerequisite_id" json:"prerequisite_id"`
}

// MapTopic is one topic on the course map.
type MapTopic struct {
	ID          string   `db:"id" json:"id"`
	Name        string   `db:"name" json:"name"`
	ParentID    *string  `db:"parent_id" json:"parent_id"`
	KeyConcepts []string `db:"key_concepts" json:"key_concepts"`
	SortOrder   int      `db:"sort_order" json:"sort_order"`
}

// CourseMap is a curriculum's topics, prerequisites and how many excerpts back each topic.
type CourseMap struct {
	Topics        []MapTopic     `json:"topics"`
	Prerequisites []Prerequisite `json:"prerequisites"`
	ExcerptCounts map[string]int `json:"excerptCounts"`
}

const sourceCols = `id, user_id, curriculum_id, kind, title, storage_path, video_url, status, error, created_at`
const chunkCols = `id, source_id, topic_id, modality, page, slide, start_sec, end_sec, content`

// Sources holds course material and its excerpts — the database half of the TS
// SourceRepository. The storage-bucket calls (uploadFile, downloadFile, signedUrl, and the file
// removal in remove) are not SQL and are not here. Always used with Admin; every query is
// filtered by the user id from the token.
type Sources struct{ q Q }

func NewSources(q Q) *Sources { return &Sources{q} }

func (r *Sources) Create(ctx context.Context, s NewSource) (*SourceRow, error) {
	return single[SourceRow](ctx, r.q, "sources.create",
		`insert into course_sources (user_id, kind, title, storage_path, video_url) values ($1, $2, $3, $4, $5)
		 returning `+sourceCols, s.UserID, s.Kind, s.Title, s.StoragePath, s.VideoURL)
}

// List is the learner's sources, newest first, each with its excerpt count.
func (r *Sources) List(ctx context.Context, userID string) ([]SourceWithCount, error) {
	return rowsOf[SourceWithCount](ctx, r.q, "sources.list",
		`select `+sourceCols+`, (select count(*)::int from source_chunks c where c.source_id = s.id) as chunk_count
		 from course_sources s where user_id = $1 order by created_at desc`, userID)
}

// Find loads a source the learner owns, or 404 SOURCE_NOT_FOUND.
func (r *Sources) Find(ctx context.Context, sourceID, userID string) (*SourceRow, error) {
	s, err := rowOf[SourceRow](ctx, r.q, "sources.find",
		`select `+sourceCols+` from course_sources where id = $1 and user_id = $2`, sourceID, userID)
	if err == nil && s == nil {
		err = httpx.E(http.StatusNotFound, "SOURCE_NOT_FOUND", "That source does not exist.")
	}
	return s, err
}

func (r *Sources) FindMany(ctx context.Context, sourceIDs []string) ([]SourceRow, error) {
	if len(sourceIDs) == 0 {
		return []SourceRow{}, nil
	}
	return rowsOf[SourceRow](ctx, r.q, "sources.findMany",
		`select `+sourceCols+` from course_sources where id = any($1::uuid[])`, sourceIDs)
}

// SetStatus sets a source's status and error message (nil clears it).
func (r *Sources) SetStatus(ctx context.Context, sourceID, status string, message *string) error {
	return exec(ctx, r.q, "sources.setStatus",
		`update course_sources set status = $2, error = $3 where id = $1`, sourceID, status, message)
}

// Remove deletes a source the learner owns (404 if not theirs) and returns the deleted row,
// so the caller can remove StoragePath from the bucket as the TS did.
func (r *Sources) Remove(ctx context.Context, sourceID, userID string) (*SourceRow, error) {
	s, err := r.Find(ctx, sourceID, userID)
	if err != nil {
		return nil, err
	}
	return s, exec(ctx, r.q, "sources.remove", `delete from course_sources where id = $1`, sourceID)
}

// InsertChunks stores a source's excerpts in one statement (the TS batched by 200 only to stay
// under PostgREST's body limit).
func (r *Sources) InsertChunks(ctx context.Context, source SourceRow, chunks []NewChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	n := len(chunks)
	modalities, contents := make([]string, n), make([]string, n)
	pages, slides, starts, ends := make([]*int, n), make([]*int, n), make([]*int, n), make([]*int, n)
	for i, c := range chunks {
		modalities[i], contents[i] = c.Modality, c.Content
		pages[i], slides[i], starts[i], ends[i] = c.Page, c.Slide, c.StartSec, c.EndSec
	}
	return exec(ctx, r.q, "sources.insertChunks",
		`insert into source_chunks (source_id, user_id, modality, page, slide, start_sec, end_sec, content)
		 select $1, $2, * from unnest($3::text[], $4::int[], $5::int[], $6::int[], $7::int[], $8::text[])`,
		source.ID, source.UserID, modalities, pages, slides, starts, ends, contents)
}

func (r *Sources) DeleteChunks(ctx context.Context, sourceID string) error {
	return exec(ctx, r.q, "sources.deleteChunks", `delete from source_chunks where source_id = $1`, sourceID)
}

// ListChunks is every excerpt of the learner's, in reading order.
func (r *Sources) ListChunks(ctx context.Context, userID string) ([]ChunkRow, error) {
	return rowsOf[ChunkRow](ctx, r.q, "sources.listChunks",
		`select `+chunkCols+` from source_chunks where user_id = $1
		 order by source_id, page nulls first, slide nulls first, start_sec nulls first`, userID)
}

// FindChunk loads an excerpt the learner owns, or 404 EXCERPT_NOT_FOUND.
func (r *Sources) FindChunk(ctx context.Context, chunkID, userID string) (*ChunkRow, error) {
	c, err := rowOf[ChunkRow](ctx, r.q, "sources.findChunk",
		`select `+chunkCols+` from source_chunks where id = $1 and user_id = $2`, chunkID, userID)
	if err == nil && c == nil {
		err = httpx.E(http.StatusNotFound, "EXCERPT_NOT_FOUND", "That excerpt does not exist.")
	}
	return c, err
}

// ChunksForTopic is up to limit excerpts tagged with a topic (TS default limit 8).
func (r *Sources) ChunksForTopic(ctx context.Context, topicID, userID string, limit int) ([]ChunkRow, error) {
	return rowsOf[ChunkRow](ctx, r.q, "sources.chunksForTopic",
		`select `+chunkCols+` from source_chunks where topic_id = $1 and user_id = $2 limit $3`, topicID, userID, limit)
}

// Search is full-text search over the learner's excerpts via search_source_chunks (TS default
// limit 6), best first, with Rank set.
func (r *Sources) Search(ctx context.Context, userID, query string, limit int) ([]ChunkRow, error) {
	type row struct {
		ChunkRow
		SearchRank float64 `db:"rank"`
	}
	rows, err := rowsOf[row](ctx, r.q, "sources.search",
		`select `+chunkCols+`, rank::float8 as rank from search_source_chunks($1, $2, $3)`, userID, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ChunkRow, len(rows))
	for i, c := range rows {
		out[i] = c.ChunkRow
		out[i].Rank = &c.SearchRank
	}
	return out, nil
}

// TagChunks sets each excerpt's topic, one statement per topic.
func (r *Sources) TagChunks(ctx context.Context, assignments []ChunkAssignment) error {
	var order []string
	byTopic := map[string][]string{}
	for _, a := range assignments {
		if _, ok := byTopic[a.TopicID]; !ok {
			order = append(order, a.TopicID)
		}
		byTopic[a.TopicID] = append(byTopic[a.TopicID], a.ChunkID)
	}
	for _, topicID := range order {
		if err := exec(ctx, r.q, "sources.tagChunks",
			`update source_chunks set topic_id = $1 where id = any($2::uuid[])`, topicID, byTopic[topicID]); err != nil {
			return err
		}
	}
	return nil
}

// AttachCurriculum points the learner's ready sources at a curriculum.
func (r *Sources) AttachCurriculum(ctx context.Context, userID, curriculumID string) error {
	return exec(ctx, r.q, "sources.attachCurriculum",
		`update course_sources set curriculum_id = $2 where user_id = $1 and status = 'ready'`, userID, curriculumID)
}

func (r *Sources) HasReadySources(ctx context.Context, userID string) (bool, error) {
	found, err := colOf[bool](ctx, r.q, "sources.hasReadySources",
		`select exists (select 1 from course_sources where user_id = $1 and status = 'ready')`, userID)
	if err != nil {
		return false, err
	}
	return found[0], nil
}

func (r *Sources) SetTopicDetails(ctx context.Context, topicID string, keyConcepts []string) error {
	return exec(ctx, r.q, "sources.setTopicDetails",
		`update topics set key_concepts = $2 where id = $1`, topicID, nonNil(keyConcepts))
}

// AddPrerequisites inserts prerequisite pairs, ignoring ones that already exist.
func (r *Sources) AddPrerequisites(ctx context.Context, pairs []Prerequisite) error {
	if len(pairs) == 0 {
		return nil
	}
	topics, prereqs := make([]string, len(pairs)), make([]string, len(pairs))
	for i, p := range pairs {
		topics[i], prereqs[i] = p.TopicID, p.PrerequisiteID
	}
	return exec(ctx, r.q, "sources.addPrerequisites",
		`insert into topic_prerequisites (topic_id, prerequisite_id) select * from unnest($1::uuid[], $2::uuid[])
		 on conflict do nothing`, topics, prereqs)
}

// CourseMap is a curriculum's topics in sort order, their prerequisites, and how many of the
// learner's excerpts are tagged to each topic.
func (r *Sources) CourseMap(ctx context.Context, curriculumID, userID string) (*CourseMap, error) {
	topics, err := rowsOf[MapTopic](ctx, r.q, "sources.courseMap",
		`select id, name, parent_id, key_concepts, sort_order from topics where curriculum_id = $1 order by sort_order`,
		curriculumID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(topics))
	for i, t := range topics {
		ids[i] = t.ID
	}
	prereqs, err := rowsOf[Prerequisite](ctx, r.q, "sources.courseMap",
		`select topic_id, prerequisite_id from topic_prerequisites where topic_id = any($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	tagged, err := colOf[string](ctx, r.q, "sources.courseMap",
		`select topic_id::text from source_chunks where user_id = $1 and topic_id = any($2::uuid[])`, userID, ids)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, id := range tagged {
		counts[id]++
	}
	return &CourseMap{Topics: topics, Prerequisites: prereqs, ExcerptCounts: counts}, nil
}
