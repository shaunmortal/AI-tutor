package content

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// conversations is ai.Conversations over the store.
type conversations struct{ r *store.AI }

func isoTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func toConversation(c *store.ConversationRow) ai.Conversation {
	return ai.Conversation{ID: c.ID, TopicID: c.TopicID, Title: c.Title, CreatedAt: isoTime(c.CreatedAt), LastMessageAt: isoTime(c.LastMessageAt)}
}

func toMessage(m *store.MessageRow) (ai.Message, error) {
	out := ai.Message{ID: m.ID, Role: m.Role, Content: m.Content, CreatedAt: isoTime(m.CreatedAt)}
	if len(m.Citations) > 0 && string(m.Citations) != "null" {
		if err := json.Unmarshal(m.Citations, &out.Citations); err != nil {
			return ai.Message{}, err
		}
	}
	return out, nil
}

func (c conversations) ListConversations(ctx context.Context, userID string) ([]ai.Conversation, error) {
	rows, err := c.r.ListConversations(ctx, userID, 30)
	if err != nil {
		return nil, err
	}
	out := make([]ai.Conversation, len(rows))
	for i := range rows {
		out[i] = toConversation(&rows[i])
	}
	return out, nil
}

func (c conversations) FindAnsweredIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	return c.r.FindAnsweredIDs(ctx, ids)
}

func (c conversations) FindConversation(ctx context.Context, id, userID string) (ai.Conversation, error) {
	row, err := c.r.FindConversation(ctx, id, userID)
	if err != nil {
		return ai.Conversation{}, err
	}
	return toConversation(row), nil
}

func (c conversations) CreateConversation(ctx context.Context, userID, title string, topicID *string) (ai.Conversation, error) {
	row, err := c.r.CreateConversation(ctx, userID, title, topicID)
	if err != nil {
		return ai.Conversation{}, err
	}
	return toConversation(row), nil
}

func (c conversations) ListMessages(ctx context.Context, conversationID string, limit int) ([]ai.Message, error) {
	rows, err := c.r.ListMessages(ctx, conversationID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ai.Message, len(rows))
	for i := range rows {
		if out[i], err = toMessage(&rows[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c conversations) AddMessage(ctx context.Context, m ai.NewMessage) (ai.Message, error) {
	nm := store.NewMessage{ConversationID: m.ConversationID, Role: m.Role, Content: m.Content}
	if m.Role != "learner" {
		in, out, model := int(m.InputTokens), int(m.OutputTokens), m.Model
		nm.InputTokens, nm.OutputTokens, nm.Model = &in, &out, &model
	}
	if m.Citations != nil {
		raw, err := json.Marshal(m.Citations)
		if err != nil {
			return ai.Message{}, err
		}
		nm.Citations = raw
	}
	row, err := c.r.AddMessage(ctx, nm)
	if err != nil {
		return ai.Message{}, err
	}
	return toMessage(row)
}

func (c conversations) FindTutorFacts(ctx context.Context, userID string, topicID *string) (ai.TutorFacts, error) {
	id := ""
	if topicID != nil {
		id = *topicID
	}
	f, err := c.r.FindTutorFacts(ctx, userID, id)
	if err != nil {
		return ai.TutorFacts{}, err
	}
	out := ai.TutorFacts{TopicName: f.TopicName, SubjectName: f.SubjectName, MasteryScore: f.MasteryScore,
		AttemptsOnTopic: f.AttemptsOnTopic, RecentMistakes: f.RecentMistakes}
	if f.RecentAccuracyPercent != nil {
		p := int(math.Round(*f.RecentAccuracyPercent))
		out.RecentAccuracyPercent = &p
	}
	return out, nil
}

// material is ai.Material over the learner's excerpts.
type material struct{ r *store.Sources }

func (m material) HasMaterial(ctx context.Context, userID string) (bool, error) {
	return m.r.HasReadySources(ctx, userID)
}

// Retrieve is the excerpts for a question, numbered for the prompt and shaped for citation chips.
func (m material) Retrieve(ctx context.Context, userID, query string, limit int) ([]ai.Citation, error) {
	rows, err := m.r.Search(ctx, userID, query, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, c := range rows {
		ids[i] = c.SourceID
	}
	srcs, err := m.r.FindMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.SourceRow{}
	for _, s := range srcs {
		byID[s.ID] = s
	}
	out := make([]ai.Citation, len(rows))
	for i, c := range rows {
		title, kind := "Course material", "pdf"
		if s, ok := byID[c.SourceID]; ok {
			title, kind = s.Title, s.Kind
		}
		out[i] = ai.Citation{N: i + 1, ChunkID: c.ID, SourceID: c.SourceID, SourceTitle: title, Kind: kind,
			Modality: c.Modality, Location: locationOf(c), Excerpt: c.Content}
	}
	return out, nil
}

// NewMaterial is the tutor's retrieval, for cmd/evalgrounding to run exactly what the tutor runs.
func NewMaterial(r *store.Sources) ai.Material { return material{r} }
