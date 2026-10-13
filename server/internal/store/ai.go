package store

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// ConversationRow is one tutor conversation.
type ConversationRow struct {
	ID            string    `db:"id" json:"id"`
	TopicID       *string   `db:"topic_id" json:"topic_id"`
	Title         string    `db:"title" json:"title"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
	LastMessageAt time.Time `db:"last_message_at" json:"last_message_at"`
}

// MessageRow is one message in a conversation.
type MessageRow struct {
	ID        string    `db:"id" json:"id"`
	Role      string    `db:"role" json:"role"` // learner | tutor
	Content   string    `db:"content" json:"content"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	// Excerpts a grounded reply cited (a JSON array); nil for ungrounded replies and learner messages.
	Citations json.RawMessage `db:"citations" json:"citations"`
}

// NewMessage is a message to add. Token counts and model must be nil on a learner message.
type NewMessage struct {
	ConversationID string
	Role           string
	Content        string
	InputTokens    *int
	OutputTokens   *int
	Model          *string
	Citations      json.RawMessage
}

// TutorFacts is where this learner stands on one topic, for grounding a tutor reply.
type TutorFacts struct {
	TopicName             *string  `json:"topicName"`
	SubjectName           *string  `json:"subjectName"`
	MasteryScore          *float64 `json:"masteryScore"`
	AttemptsOnTopic       int      `json:"attemptsOnTopic"`
	RecentAccuracyPercent *float64 `json:"recentAccuracyPercent"`
	RecentMistakes        []string `json:"recentMistakes"`
}

const conversationCols = `id, topic_id, title, created_at, last_message_at`
const messageCols = `id, role, content, created_at, citations`

// AI holds tutor conversations and the learner state a reply is grounded in. Writes need
// Admin: learners get SELECT only.
type AI struct{ q Q }

func NewAI(q Q) *AI { return &AI{q} }

// ListConversations is the learner's conversations, most recent first (TS default limit 30).
func (r *AI) ListConversations(ctx context.Context, userID string, limit int) ([]ConversationRow, error) {
	return rowsOf[ConversationRow](ctx, r.q, "ai.listConversations",
		`select `+conversationCols+` from ai_conversations where user_id = $1 order by last_message_at desc limit $2`,
		userID, limit)
}

// FindConversation loads a conversation the learner owns, or 404 CONVERSATION_NOT_FOUND —
// not 403, since saying it exists but is not theirs is itself a leak.
func (r *AI) FindConversation(ctx context.Context, conversationID, userID string) (*ConversationRow, error) {
	c, err := rowOf[ConversationRow](ctx, r.q, "ai.findConversation",
		`select `+conversationCols+` from ai_conversations where id = $1 and user_id = $2`, conversationID, userID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, httpx.E(http.StatusNotFound, "CONVERSATION_NOT_FOUND", "That conversation does not exist.")
	}
	return c, nil
}

// FindAnsweredIDs is which of these conversations have a tutor reply.
func (r *AI) FindAnsweredIDs(ctx context.Context, conversationIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(conversationIDs) == 0 {
		return out, nil
	}
	ids, err := colOf[string](ctx, r.q, "ai.findAnsweredIds",
		`select conversation_id::text from ai_messages where conversation_id = any($1::uuid[]) and role = 'tutor'`,
		conversationIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// ListMessages is a conversation's latest limit messages, oldest first. (The TS took the
// oldest 50, so a long thread replayed its opening turns to the model as "recent history".)
func (r *AI) ListMessages(ctx context.Context, conversationID string, limit int) ([]MessageRow, error) {
	return rowsOf[MessageRow](ctx, r.q, "ai.listMessages",
		`select * from (select `+messageCols+` from ai_messages where conversation_id = $1
		 order by created_at desc limit $2) latest order by created_at`,
		conversationID, limit)
}

func (r *AI) CreateConversation(ctx context.Context, userID, title string, topicID *string) (*ConversationRow, error) {
	return single[ConversationRow](ctx, r.q, "ai.createConversation",
		`insert into ai_conversations (user_id, title, topic_id) values ($1, $2, $3) returning `+conversationCols,
		userID, title, topicID)
}

func (r *AI) AddMessage(ctx context.Context, m NewMessage) (*MessageRow, error) {
	return single[MessageRow](ctx, r.q, "ai.addMessage",
		`insert into ai_messages (conversation_id, role, content, input_tokens, output_tokens, model, citations)
		 values ($1, $2, $3, $4, $5, $6, $7) returning `+messageCols,
		m.ConversationID, m.Role, m.Content, m.InputTokens, m.OutputTokens, m.Model, m.Citations)
}

// FindTutorFacts is the learner's state on one topic: names, mastery, and the bodies (never
// the answers) of up to three recently missed questions. topicID "" means no topic.
func (r *AI) FindTutorFacts(ctx context.Context, userID, topicID string) (TutorFacts, error) {
	empty := TutorFacts{RecentMistakes: []string{}}
	if topicID == "" {
		return empty, nil
	}

	type topicRow struct {
		Name       string  `db:"name"`
		ParentID   *string `db:"parent_id"`
		ParentName *string `db:"parent_name"`
	}
	topic, err := rowOf[topicRow](ctx, r.q, "ai.findTutorFacts",
		`select t.name, t.parent_id, p.name as parent_name from topics t left join topics p on p.id = t.parent_id
		 where t.id = $1`, topicID)
	if err != nil {
		return TutorFacts{}, err
	}
	if topic == nil {
		return empty, nil
	}

	type masteryRow struct {
		MasteryScore   float64  `db:"mastery_score"`
		TotalAttempts  int      `db:"total_attempts"`
		RecentAccuracy *float64 `db:"recent_accuracy"`
	}
	mastery, err := rowOf[masteryRow](ctx, r.q, "ai.findTutorFacts",
		`select mastery_score, total_attempts, recent_accuracy from concept_mastery where user_id = $1 and topic_id = $2`,
		userID, topicID)
	if err != nil {
		return TutorFacts{}, err
	}

	wrong, err := colOf[string](ctx, r.q, "ai.findTutorFacts",
		`select question_id::text from question_attempts where user_id = $1 and topic_id = $2 and not is_correct
		 order by attempted_at desc limit 3`, userID, topicID)
	if err != nil {
		return TutorFacts{}, err
	}
	mistakes := []string{}
	if wrongIDs := distinct(wrong); len(wrongIDs) > 0 {
		// Question bodies only — never the answers.
		if mistakes, err = colOf[string](ctx, r.q, "ai.findTutorFacts",
			`select body from questions where id = any($1::uuid[])`, wrongIDs); err != nil {
			return TutorFacts{}, err
		}
	}

	facts := TutorFacts{TopicName: &topic.Name, RecentMistakes: mistakes}
	if topic.ParentID != nil {
		facts.SubjectName = topic.ParentName
	}
	if mastery != nil {
		facts.MasteryScore = &mastery.MasteryScore
		facts.AttemptsOnTopic = mastery.TotalAttempts
		if mastery.RecentAccuracy != nil {
			facts.RecentAccuracyPercent = ptr(math.Round(*mastery.RecentAccuracy))
		}
	}
	return facts, nil
}
