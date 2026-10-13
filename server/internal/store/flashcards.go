package store

import (
	"context"
	"net/http"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// FlashcardRow is one stored revision card.
type FlashcardRow struct {
	ID            string  `db:"id" json:"id"`
	TopicID       string  `db:"topic_id" json:"topicId"`
	Front         string  `db:"front" json:"front"`
	Back          string  `db:"back" json:"back"`
	SourceChunkID *string `db:"source_chunk_id" json:"sourceChunkId"`
}

// NewFlashcard is a card to store.
type NewFlashcard struct {
	Front, Back   string
	SourceChunkID string
}

// RevisionTopic is a topic worth revising from cards: due in the schedule, or weak.
type RevisionTopic struct {
	TopicID      string   `db:"topic_id" json:"topicId"`
	Name         string   `db:"name" json:"name"`
	Reason       string   `db:"reason" json:"reason"` // due | weak
	DueOn        *string  `db:"due_on" json:"dueOn"`
	MasteryScore *float64 `db:"mastery_score" json:"masteryScore"`
}

// Flashcards holds revision cards. Writes need Admin; every query filters by user id.
type Flashcards struct{ q Q }

func NewFlashcards(q Q) *Flashcards { return &Flashcards{q} }

// RevisionTopics is up to limit topics that have the learner's own excerpts and are due for
// review by today (longest overdue first) or weak (mastery below weakBelow, weakest first).
// Topics without excerpts are left out: a card that cannot cite its source is not written.
func (r *Flashcards) RevisionTopics(ctx context.Context, userID, today string, weakBelow float64, limit int) ([]RevisionTopic, error) {
	return rowsOf[RevisionTopic](ctx, r.q, "flashcards.revisionTopics",
		`select x.topic_id, t.name, x.reason, x.due_on, m.mastery_score
		 from (select topic_id, 'due' as reason, due_on::text as due_on, 0 as grp
		         from revision_schedule where user_id = $1 and due_on <= $2
		       union all
		       select topic_id, 'weak', null, 1 from concept_mastery
		        where user_id = $1 and mastery_score < $3
		          and topic_id not in (select topic_id from revision_schedule where user_id = $1 and due_on <= $2)) x
		 join topics t on t.id = x.topic_id
		 left join concept_mastery m on m.user_id = $1 and m.topic_id = x.topic_id
		 where exists (select 1 from source_chunks c where c.user_id = $1 and c.topic_id = x.topic_id)
		 order by x.grp, x.due_on, m.mastery_score nulls first, t.name
		 limit $4`, userID, today, weakBelow, limit)
}

// ForTopics is the learner's cards on these topics, oldest first.
func (r *Flashcards) ForTopics(ctx context.Context, userID string, topicIDs []string) ([]FlashcardRow, error) {
	return rowsOf[FlashcardRow](ctx, r.q, "flashcards.forTopics",
		`select id, topic_id, front, back, source_chunk_id from flashcards
		 where user_id = $1 and topic_id = any($2::uuid[]) order by created_at, id`, userID, nonNil(topicIDs))
}

// Insert stores a topic's cards in one statement.
func (r *Flashcards) Insert(ctx context.Context, userID, topicID string, cards []NewFlashcard) error {
	fronts, backs, chunks := make([]string, len(cards)), make([]string, len(cards)), make([]string, len(cards))
	for i, c := range cards {
		fronts[i], backs[i], chunks[i] = c.Front, c.Back, c.SourceChunkID
	}
	return exec(ctx, r.q, "flashcards.insert",
		`insert into flashcards (user_id, topic_id, front, back, source_chunk_id)
		 select $1, $2, * from unnest($3::text[], $4::text[], $5::uuid[])`, userID, topicID, fronts, backs, chunks)
}

// Find loads a card the learner owns, or 404 FLASHCARD_NOT_FOUND.
func (r *Flashcards) Find(ctx context.Context, id, userID string) (*FlashcardRow, error) {
	c, err := rowOf[FlashcardRow](ctx, r.q, "flashcards.find",
		`select id, topic_id, front, back, source_chunk_id from flashcards where id = $1 and user_id = $2`, id, userID)
	if err == nil && c == nil {
		err = httpx.E(http.StatusNotFound, "FLASHCARD_NOT_FOUND", "That card does not exist.")
	}
	return c, err
}

// ChunksByIDs is the learner's excerpts with these ids.
func (r *Sources) ChunksByIDs(ctx context.Context, userID string, ids []string) ([]ChunkRow, error) {
	return rowsOf[ChunkRow](ctx, r.q, "sources.chunksByIds",
		`select `+chunkCols+` from source_chunks where user_id = $1 and id = any($2::uuid[])`, userID, nonNil(ids))
}
