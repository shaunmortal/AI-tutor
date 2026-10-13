package store

import (
	"context"
	"fmt"
	"strconv"
)

// CitedChunk is an excerpt with the source it belongs to: what a citation chip shows.
type CitedChunk struct {
	ChunkRow
	SourceTitle string `db:"source_title"`
	Kind        string `db:"kind"` // pdf | slides | video
}

// FindCited loads the learner's own excerpts by id, keyed by id. Another learner's excerpt is
// absent, so a shared question never shows somebody else's material.
func (r *Sources) FindCited(ctx context.Context, userID string, chunkIDs []string) (map[string]CitedChunk, error) {
	out := map[string]CitedChunk{}
	if len(chunkIDs) == 0 {
		return out, nil
	}
	rows, err := rowsOf[CitedChunk](ctx, r.q, "sources.findCited",
		`select c.id, c.source_id, c.topic_id, c.modality, c.page, c.slide, c.start_sec, c.end_sec, c.content,
		        s.title as source_title, s.kind
		 from source_chunks c join course_sources s on s.id = c.source_id
		 where c.id = any($1::uuid[]) and c.user_id = $2`, distinct(chunkIDs), userID)
	for _, c := range rows {
		out[c.ID] = c
	}
	return out, err
}

// Location is where in its source an excerpt sits: "p. 42", "slide 7" or "12:05".
func (c ChunkRow) Location() string {
	switch {
	case c.StartSec != nil:
		h, m, s := *c.StartSec/3600, *c.StartSec%3600/60, *c.StartSec%60
		if h > 0 {
			return fmt.Sprintf("%d:%02d:%02d", h, m, s)
		}
		return fmt.Sprintf("%d:%02d", m, s)
	case c.Slide != nil:
		return "slide " + strconv.Itoa(*c.Slide)
	case c.Page != nil:
		return "p. " + strconv.Itoa(*c.Page)
	}
	return ""
}
