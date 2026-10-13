package practice

import (
	"context"
	"net/http"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// The post-test review and the misconception report: what went wrong, why, and where in the
// learner's own material to go and look. Elevated client; every read filters by user id.

// citation is a course excerpt, shaped like the tutor's citations so the same chip renders it.
// Opening it goes through GET /sources/excerpts/{id}/open.
type citation struct {
	N           int    `json:"n"`
	ChunkID     string `json:"chunkId"`
	SourceID    string `json:"sourceId"`
	SourceTitle string `json:"sourceTitle"`
	Kind        string `json:"kind"`
	Modality    string `json:"modality"`
	Location    string `json:"location"`
	Excerpt     string `json:"excerpt"`
}

// excerptChars caps the excerpt text a chip carries; the open link shows the rest.
const excerptChars = 600

// citations resolves excerpt ids the learner owns into citations, keyed by id.
func citations(ctx context.Context, d *app.Deps, userID string, chunkIDs []string) (map[string]*citation, error) {
	cited, err := store.NewSources(d.DB.Admin).FindCited(ctx, userID, chunkIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*citation, len(cited))
	for id, c := range cited {
		excerpt := c.Content
		if r := []rune(excerpt); len(r) > excerptChars {
			excerpt = string(r[:excerptChars]) + "…"
		}
		out[id] = &citation{N: 1, ChunkID: c.ID, SourceID: c.SourceID, SourceTitle: c.SourceTitle, Kind: c.Kind,
			Modality: c.Modality, Location: c.Location(), Excerpt: excerpt}
	}
	return out, nil
}

// reviewTest is every question of a submitted test with the answer key, in paper order.
func reviewTest(ctx context.Context, d *app.Deps, userID, testID string, byTopic []namedTopicScore) ([]reviewItem, error) {
	rows, err := store.NewMockTests(d.DB.Admin).FindReview(ctx, testID)
	if err != nil {
		return nil, err
	}
	var chunkIDs []string
	for _, row := range rows {
		if row.SourceChunkID != nil {
			chunkIDs = append(chunkIDs, *row.SourceChunkID)
		}
	}
	cited, err := citations(ctx, d, userID, chunkIDs)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, t := range byTopic {
		names[t.TopicID] = t.Name
	}
	out := make([]reviewItem, len(rows))
	for i, row := range rows {
		status := "skipped"
		switch {
		case row.IsCorrect != nil && *row.IsCorrect:
			status = "correct"
		case row.IsCorrect != nil:
			status = "wrong"
		case given(row.GivenAnswer):
			status = "unmarked"
		}
		item := reviewItem{QuestionID: row.QuestionID, TopicID: row.TopicID, TopicName: names[deref(row.TopicID)],
			QuestionType: row.QuestionType, Body: row.Body, Options: row.Options, Status: status,
			GivenAnswer: row.GivenAnswer, CorrectAnswer: row.CorrectAnswer, Explanation: row.Explanation,
			Feedback: row.Feedback, Misconception: row.Misconception}
		if row.SourceChunkID != nil {
			item.Source = cited[*row.SourceChunkID]
		}
		if !given(item.GivenAnswer) {
			item.GivenAnswer = nil
		}
		out[i] = item
	}
	return out, nil
}

// topicMisconceptions is one topic's recurring misconceptions.
type topicMisconceptions struct {
	TopicID        string              `json:"topicId"`
	TopicName      string              `json:"topicName"`
	MasteryScore   *float64            `json:"masteryScore"`
	Misconceptions []misconceptionItem `json:"misconceptions"`
}

type misconceptionItem struct {
	Label       string    `json:"label"`
	Occurrences int       `json:"occurrences"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	// Source is where in the learner's own material to revisit it, when they have some.
	Source *citation `json:"source"`
}

// Report sizes: a few labels per topic is a to-do list; a dozen is noise.
const (
	misconceptionsPerTopic = 3
	misconceptionTopics    = 5
)

// misconceptionReport groups the learner's top misconceptions by topic, weakest topic first.
// topicIDs nil means every topic.
func misconceptionReport(ctx context.Context, d *app.Deps, userID string, topicIDs []string) ([]topicMisconceptions, error) {
	rows, err := store.NewMistakes(d.DB.Admin).Top(ctx, userID, topicIDs, misconceptionsPerTopic,
		misconceptionsPerTopic*misconceptionTopics)
	if err != nil {
		return nil, err
	}
	var chunkIDs []string
	for _, row := range rows {
		if row.SourceChunkID != nil {
			chunkIDs = append(chunkIDs, *row.SourceChunkID)
		}
	}
	cited, err := citations(ctx, d, userID, chunkIDs)
	if err != nil {
		return nil, err
	}
	out := []topicMisconceptions{}
	for _, row := range rows {
		if len(out) == 0 || out[len(out)-1].TopicID != row.TopicID {
			out = append(out, topicMisconceptions{TopicID: row.TopicID, TopicName: row.TopicName,
				MasteryScore: row.MasteryScore, Misconceptions: []misconceptionItem{}})
		}
		item := misconceptionItem{Label: row.Label, Occurrences: row.Occurrences, LastSeenAt: row.LastSeenAt}
		if row.SourceChunkID != nil {
			item.Source = cited[*row.SourceChunkID]
		}
		last := &out[len(out)-1]
		last.Misconceptions = append(last.Misconceptions, item)
	}
	return out, nil
}

// weakTopicMisconceptions is the report for the topics a test went badly on.
func weakTopicMisconceptions(ctx context.Context, d *app.Deps, userID string, byTopic []namedTopicScore) ([]topicMisconceptions, error) {
	var weak []string
	for _, t := range byTopic {
		if t.Percent < assessment.MockPassPercent {
			weak = append(weak, t.TopicID)
		}
	}
	if len(weak) == 0 {
		return []topicMisconceptions{}, nil
	}
	return misconceptionReport(ctx, d, userID, weak)
}

// GET /misconceptions — the dashboard's "what you keep getting wrong".
func listMisconceptions(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		report, err := misconceptionReport(r.Context(), d, auth.UserID(r), nil)
		if err != nil {
			return err
		}
		httpx.OK(w, http.StatusOK, map[string]any{"topics": report})
		return nil
	}
}
