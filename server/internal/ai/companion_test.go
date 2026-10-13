package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// In-memory stand-ins for the store, so Ask can be driven end to end with a fake model.
type memConversations struct {
	topic    map[string]string
	checks   []CheckQuestion
	messages []NewMessage
	language string
}

func (m *memConversations) ListConversations(context.Context, string) ([]Conversation, error) {
	return nil, nil
}
func (m *memConversations) FindAnsweredIDs(context.Context, []string) (map[string]bool, error) {
	return nil, nil
}
func (m *memConversations) FindConversation(_ context.Context, id, _ string) (Conversation, error) {
	return Conversation{ID: id}, nil
}
func (m *memConversations) CreateConversation(_ context.Context, _, title string, topicID *string) (Conversation, error) {
	return Conversation{ID: "conv", Title: title, TopicID: topicID}, nil
}
func (m *memConversations) ListMessages(context.Context, string, int) ([]Message, error) {
	return nil, nil
}
func (m *memConversations) AddMessage(_ context.Context, n NewMessage) (Message, error) {
	m.messages = append(m.messages, n)
	return Message{Role: n.Role, Content: n.Content, Citations: n.Citations}, nil
}
func (m *memConversations) FindTutorFacts(context.Context, string, *string) (TutorFacts, error) {
	return TutorFacts{PreferredLanguage: m.language}, nil
}
func (m *memConversations) SetTopic(_ context.Context, conv, topic string) error {
	if m.topic == nil {
		m.topic = map[string]string{}
	}
	m.topic[conv] = topic
	return nil
}
func (m *memConversations) SaveCheck(_ context.Context, _ string, c CheckQuestion) (string, error) {
	m.checks = append(m.checks, c)
	return "check-1", nil
}

type memMaterial struct {
	has     bool
	queries []string
	found   []Citation
}

func (m *memMaterial) HasMaterial(context.Context, string) (bool, error) { return m.has, nil }
func (m *memMaterial) Retrieve(_ context.Context, _, query string, _ int) ([]Citation, error) {
	m.queries = append(m.queries, query)
	return m.found, nil
}

func tagged(n int, topic string) Citation {
	c := excerpt(n)
	c.TopicID = &topic
	return c
}

const groundedWithCheck = `{"grounding":"source","answer":"A key identifies a row [1].","outsideKnowledge":"",
 "check":{"question":"What does a key do?","options":["Identifies a row","Sorts rows","Deletes rows","Joins tables"],
 "correctIndex":0,"explanation":"A key identifies a row.","sourceExcerpt":1}}`

func TestAskWithoutMaterialIsLabelledGeneralKnowledge(t *testing.T) {
	conv := &memConversations{}
	tutor := &Tutor{AI: NewFake(func(Request) (string, error) { return "A heap is a tree.", nil }),
		Conversations: conv, Material: &memMaterial{has: false}}
	reply, err := tutor.Ask(context.Background(), AskInput{UserID: "u", Question: "What is a heap?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply.Answer, "General knowledge, not from your course material") ||
		!strings.Contains(reply.Answer, "Sources page") || !strings.HasSuffix(reply.Answer, "A heap is a tree.") {
		t.Fatalf("answer: %q", reply.Answer)
	}
	if reply.Grounding != nil || reply.Check != nil || len(reply.Citations) != 0 {
		t.Fatalf("ungrounded reply carried grounding, a check or citations: %+v", reply)
	}
	// Stored with the label, so the thread reads the same when reopened.
	if stored := conv.messages[len(conv.messages)-1].Content; stored != reply.Answer {
		t.Fatalf("stored %q", stored)
	}
}

func TestAskWithMaterialAddsCheckAndRecordsTopic(t *testing.T) {
	conv := &memConversations{}
	m := &memMaterial{has: true, found: []Citation{tagged(1, "keys"), tagged(2, "joins")}}
	tutor := &Tutor{AI: NewFake(func(Request) (string, error) { return groundedWithCheck, nil }), Conversations: conv, Material: m}
	reply, err := tutor.Ask(context.Background(), AskInput{UserID: "u", Question: "What is a key?"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.TopicID == nil || *reply.TopicID != "keys" || conv.topic["conv"] != "keys" {
		t.Fatalf("topic not recorded: %v %v", reply.TopicID, conv.topic)
	}
	if reply.Check == nil || reply.Check.QuestionID != "check-1" || len(reply.Check.Options) != 4 || reply.Check.Citation.ChunkID != "chunk-1" {
		t.Fatalf("check: %+v", reply.Check)
	}
	if len(conv.checks) != 1 || conv.checks[0].CorrectIndex != 0 || conv.checks[0].TopicID != "keys" || conv.checks[0].ChunkID != "chunk-1" {
		t.Fatalf("saved: %+v", conv.checks)
	}

	// A conversation that already has a topic keeps it.
	conv2 := &memConversations{}
	topic := "given"
	tutor.Conversations = conv2
	reply, err = tutor.Ask(context.Background(), AskInput{UserID: "u", Question: "What is a key?", TopicID: &topic})
	if err != nil || *reply.TopicID != "given" || len(conv2.topic) != 0 {
		t.Fatalf("given topic overwritten: %v %v %v", err, reply.TopicID, conv2.topic)
	}
}

func TestValidCheck(t *testing.T) {
	cited := []Citation{tagged(1, "keys"), excerpt(2)}
	ok := RawCheck{Question: "Q?", Options: []string{"a", "b", "c", "d"}, CorrectIndex: 2, SourceExcerpt: 1}
	if c, cite := ValidCheck(&ok, cited); c == nil || c.TopicID != "keys" || cite.N != 1 {
		t.Fatalf("valid check rejected: %+v", c)
	}
	for name, mod := range map[string]func(*RawCheck){
		"blank question":        func(r *RawCheck) { r.Question = " " },
		"three options":         func(r *RawCheck) { r.Options = r.Options[:3] },
		"duplicate options":     func(r *RawCheck) { r.Options = []string{"a", "a", "c", "d"} },
		"index out of range":    func(r *RawCheck) { r.CorrectIndex = 4 },
		"uncited excerpt":       func(r *RawCheck) { r.SourceExcerpt = 3 },
		"excerpt with no topic": func(r *RawCheck) { r.SourceExcerpt = 2 },
	} {
		raw := ok
		raw.Options = append([]string{}, ok.Options...)
		mod(&raw)
		if c, _ := ValidCheck(&raw, cited); c != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if c, _ := ValidCheck(nil, cited); c != nil {
		t.Error("nil accepted")
	}
}

func TestSearchQueryTranslates(t *testing.T) {
	ctx := context.Background()
	var translated []string
	fake := NewFake(func(r Request) (string, error) {
		if r.System != TranslateSystem {
			return groundedWithCheck, nil
		}
		translated = append(translated, r.Prompt)
		if r.MaxTokens > 300 || r.Quality != Standard {
			t.Errorf("translation should be cheap: %+v", r)
		}
		return "what is normalisation", nil
	})

	// Devanagari: searched in both languages.
	m := &memMaterial{has: true, found: []Citation{tagged(1, "keys")}}
	if _, err := AnswerFromMaterial(ctx, fake, m, "u", "नॉर्मलाइज़ेशन क्या है?", TutorContext{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(translated) != 1 || m.queries[0] != "नॉर्मलाइज़ेशन क्या है? what is normalisation" {
		t.Fatalf("query %q after %d translations", m.queries, len(translated))
	}

	// English: no translation call.
	if q := SearchQuery(ctx, fake, "What is normalisation?", "english"); q != "What is normalisation?" || len(translated) != 1 {
		t.Fatalf("english translated: %q", q)
	}
	// Hinglish in Latin script, for a learner who studies in Hinglish: translated.
	if q := SearchQuery(ctx, fake, "normalisation kya hota hai", "hinglish"); !strings.HasSuffix(q, " what is normalisation") {
		t.Fatalf("hinglish: %q", q)
	}
	// A failed translation falls back to the question alone.
	failing := NewFake(func(Request) (string, error) { return "", errors.New("down") })
	if q := SearchQuery(ctx, failing, "नॉर्मलाइज़ेशन", ""); q != "नॉर्मलाइज़ेशन" {
		t.Fatalf("fallback: %q", q)
	}
}

func TestNeedsTranslation(t *testing.T) {
	for q, want := range map[string]bool{
		"What is a heap?":      false,
		"हीप क्या है?":         true,
		"heap क्या होता है":    true, // more Devanagari letters than Latin
		"What is a heap? क्या": false,
		"1 + 1 = ?":            false,
	} {
		if got := NeedsTranslation(q, "english"); got != want {
			t.Errorf("NeedsTranslation(%q) = %v", q, got)
		}
	}
}

func TestTutorPromptLanguage(t *testing.T) {
	if p := TutorPrompt("q", TutorContext{TutorFacts: TutorFacts{PreferredLanguage: "hinglish"}}); !strings.Contains(p, "Hinglish") {
		t.Fatalf("prompt: %s", p)
	}
	if p := TutorPrompt("q", TutorContext{TutorFacts: TutorFacts{PreferredLanguage: "english"}}); strings.Contains(p, "prefer") {
		t.Fatalf("english prompt mentions a preference: %s", p)
	}
}
