package store

import (
	"context"
	"net/http"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// SetConversationTopic records what a conversation is about. Admin.
func (r *AI) SetConversationTopic(ctx context.Context, conversationID, topicID string) error {
	return exec(ctx, r.q, "ai.setConversationTopic",
		`update ai_conversations set topic_id = $2 where id = $1`, conversationID, topicID)
}

// FindPreferredLanguage is english, hindi or hinglish ("english" when the profile is unreadable).
func (r *AI) FindPreferredLanguage(ctx context.Context, userID string) (string, error) {
	langs, err := colOf[string](ctx, r.q, "ai.findPreferredLanguage",
		`select preferred_language from profiles where id = $1`, userID)
	if err != nil || len(langs) == 0 {
		return "english", err
	}
	return langs[0], nil
}

// SaveCheck stores a tutor's quick check as an unverified question created by the learner, so it
// never enters anyone's practice bank, and returns its id. Asked twice word for word on the same
// topic, the existing question is reused (questions are unique per topic and body).
func (r *AI) SaveCheck(ctx context.Context, userID string, g GeneratedQuestion) (string, error) {
	g.IsVerified, g.CreatedBy = false, &userID
	id, err := NewQuestions(r.q).InsertGenerated(ctx, g)
	if err != nil || id != nil {
		if id == nil {
			return "", err
		}
		return *id, nil
	}
	ids, err := colOf[string](ctx, r.q, "ai.saveCheck",
		`select id::text from questions where topic_id = $1 and md5(body) = md5($2)`, g.TopicID, g.Body)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// FindCheckForGrading is a question this learner may answer as a quick check: one the tutor
// wrote for them, or any verified question. 404 QUESTION_NOT_FOUND otherwise.
func (r *Attempts) FindCheckForGrading(ctx context.Context, questionID, userID string) (*QuestionForGrading, error) {
	q, err := rowOf[QuestionForGrading](ctx, r.q, "attempts.findCheckForGrading",
		`select `+gradingCols+` from questions where id = $1 and (is_verified or created_by = $2)`, questionID, userID)
	if err == nil && q == nil {
		err = httpx.E(http.StatusNotFound, "QUESTION_NOT_FOUND", "That question does not exist.")
	}
	return q, err
}
