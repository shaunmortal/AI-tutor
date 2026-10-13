package ai

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Citation is an excerpt of the learner's course material, numbered as the prompt showed it
// and shaped for a citation chip. Built by the sources layer, which owns retrieval.
type Citation struct {
	N           int    `json:"n"`
	ChunkID     string `json:"chunkId"`
	SourceID    string `json:"sourceId"`
	SourceTitle string `json:"sourceTitle"`
	Kind        string `json:"kind"`     // pdf, slides or video
	Modality    string `json:"modality"` // text, figure or transcript
	// "p. 42", "slide 7", "12:05": what the chip shows.
	Location string `json:"location"`
	Excerpt  string `json:"excerpt"`
	// The topic the course map tagged the excerpt with; nil before the course is organised.
	TopicID *string `json:"topicId,omitempty"`
}

// Grounding says how much of an answer came from the learner's own material.
type Grounding string

const (
	GroundingSource  Grounding = "source"
	GroundingPartial Grounding = "partial"
	GroundingNone    Grounding = "none"
)

const (
	// HistoryTurns is how many earlier turns to replay: enough to follow up, short enough to stay cheap.
	HistoryTurns = 8
	// MaxQuestion caps a phone-typed question. Longer is an essay, not a doubt.
	MaxQuestion = 2000
	// MessagesShown is how much of a thread GetConversation returns (the latest).
	MessagesShown = 50
	// excerptsPerAnswer is how many excerpts the model is shown per question.
	excerptsPerAnswer = 6
)

// Conversation is a stored tutor thread.
type Conversation struct {
	ID            string  `json:"id"`
	TopicID       *string `json:"topicId"`
	Title         string  `json:"title"`
	CreatedAt     string  `json:"createdAt"`
	LastMessageAt string  `json:"lastMessageAt"`
}

// Message is a stored turn. Citations is nil for learner messages and ungrounded replies.
type Message struct {
	ID        string
	Role      string // "learner" or "tutor"
	Content   string
	CreatedAt string
	Citations []Citation
}

// NewMessage is a turn to store. Token counts and model are zero for a learner message (the
// store writes them as NULL); Citations nil is stored as NULL.
type NewMessage struct {
	ConversationID string
	Role           string
	Content        string
	InputTokens    int64
	OutputTokens   int64
	Model          string
	Citations      []Citation
}

// Conversations is the storage the tutor needs. FindConversation must check ownership and
// return a 404-style error for a conversation that is not the user's.
type Conversations interface {
	ListConversations(ctx context.Context, userID string) ([]Conversation, error)
	// FindAnsweredIDs reports which conversations have a tutor reply.
	FindAnsweredIDs(ctx context.Context, conversationIDs []string) (map[string]bool, error)
	FindConversation(ctx context.Context, conversationID, userID string) (Conversation, error)
	CreateConversation(ctx context.Context, userID, title string, topicID *string) (Conversation, error)
	// ListMessages returns a conversation's latest limit messages, oldest first.
	ListMessages(ctx context.Context, conversationID string, limit int) ([]Message, error)
	AddMessage(ctx context.Context, m NewMessage) (Message, error)
	FindTutorFacts(ctx context.Context, userID string, topicID *string) (TutorFacts, error)
	// SetTopic records what a conversation turned out to be about.
	SetTopic(ctx context.Context, conversationID, topicID string) error
	// SaveCheck stores a quick check question for this learner and returns its id.
	SaveCheck(ctx context.Context, userID string, c CheckQuestion) (string, error)
}

// Material is the learner's course material, as far as the tutor needs it.
type Material interface {
	HasMaterial(ctx context.Context, userID string) (bool, error)
	// Retrieve returns up to limit excerpts for a query, numbered from 1.
	Retrieve(ctx context.Context, userID, query string, limit int) ([]Citation, error)
}

// Tutor answers learner questions and keeps the threads.
type Tutor struct {
	AI            *Client
	Conversations Conversations
	// Material, when set, grounds answers in the learner's own course material once they have some.
	Material Material
}

// ConversationSummary is one row of the history list.
type ConversationSummary struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	TopicID       *string `json:"topicId"`
	LastMessageAt string  `json:"lastMessageAt"`
	// False when the question was asked but never answered (an outage, or a call that failed
	// after the question was saved). The list says so rather than showing an ordinary thread.
	Answered bool `json:"answered"`
}

func (t *Tutor) ListConversations(ctx context.Context, userID string) ([]ConversationSummary, error) {
	rows, err := t.Conversations.ListConversations(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	answered, err := t.Conversations.FindAnsweredIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ConversationSummary, len(rows))
	for i, r := range rows {
		out[i] = ConversationSummary{ID: r.ID, Title: r.Title, TopicID: r.TopicID, LastMessageAt: r.LastMessageAt, Answered: answered[r.ID]}
	}
	return out, nil
}

// MessageView is a message as the client renders it.
type MessageView struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	CreatedAt string     `json:"createdAt"`
	Citations []Citation `json:"citations"`
}

// ConversationView is a whole thread.
type ConversationView struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	TopicID  *string       `json:"topicId"`
	Messages []MessageView `json:"messages"`
}

func (t *Tutor) GetConversation(ctx context.Context, conversationID, userID string) (ConversationView, error) {
	c, err := t.Conversations.FindConversation(ctx, conversationID, userID)
	if err != nil {
		return ConversationView{}, err
	}
	msgs, err := t.Conversations.ListMessages(ctx, c.ID, MessagesShown)
	if err != nil {
		return ConversationView{}, err
	}
	views := make([]MessageView, len(msgs))
	for i, m := range msgs {
		views[i] = toView(m)
	}
	return ConversationView{ID: c.ID, Title: c.Title, TopicID: c.TopicID, Messages: views}, nil
}

// AskInput is one learner question.
type AskInput struct {
	UserID   string
	Question string
	// ConversationID continues a thread; empty starts a new one.
	ConversationID string
	// TopicID is the topic in view, so the reply is grounded in what they know about it.
	TopicID *string
	// CurrentQuestion is the question on screen, when they asked from the practice page.
	CurrentQuestion *string
}

// TutorUsage makes the feature's cost measurable rather than guessed at.
type TutorUsage struct {
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Model        string `json:"model"`
}

// TutorReply is the answer to Ask.
type TutorReply struct {
	ConversationID string `json:"conversationId"`
	Title          string `json:"title"`
	Answer         string `json:"answer"`
	// Source, partial or none when the learner has course material; nil when they have none
	// and the tutor answered from general knowledge.
	Grounding *Grounding `json:"grounding"`
	// The excerpts the answer cites, numbered as they appear in it.
	Citations []Citation `json:"citations"`
	// Both messages, so the client renders the exchange without a second request.
	Messages []MessageView `json:"messages"`
	Usage    TutorUsage    `json:"usage"`
	// The topic the conversation is about, when known (given, or inferred from the citations).
	TopicID *string `json:"topicId"`
	// One multiple-choice question on what was just explained; nil when none was written.
	// Answered through POST /ai/checks/{questionId}/answer, which grades it and records it.
	Check *QuickCheck `json:"check"`
}

// CheckQuestion is a validated quick check, ready to store as an (unverified) question.
type CheckQuestion struct {
	TopicID      string
	Body         string
	Options      []string
	CorrectIndex int
	Explanation  string
	ChunkID      string
}

// QuickCheck is a check question as the learner sees it: no answer until they commit to one.
type QuickCheck struct {
	QuestionID string   `json:"questionId"`
	Body       string   `json:"body"`
	Options    []string `json:"options"`
	Citation   Citation `json:"citation"`
}

// Answer is a tutor reply before it is stored.
type Answer struct {
	Text string
	// Nil for a free (ungrounded) answer.
	Grounding *Grounding
	Citations []Citation
	// Everything retrieved, cited or not: what context precision is measured against.
	// Only set by AnswerFromMaterial.
	Retrieved []Citation
	Usage     Usage
	Model     string
	// A quick check on the answer, already validated against the cited excerpts. Only set by
	// AnswerFromMaterial.
	Check *CheckQuestion
	// The excerpt Check was written from.
	CheckCitation Citation
}

// Ask is one question, one answer, both stored.
//
// Order matters. The learner's message is written before the model is called, so a question
// asked during an outage is not lost: they can reload, see it and ask again. The reply is
// written after, so a failed call leaves a conversation whose last message is unanswered
// rather than one that pretends nothing was asked.
func (t *Tutor) Ask(ctx context.Context, in AskInput) (TutorReply, error) {
	question := truncate(jsTrim(in.Question), MaxQuestion)

	var conv Conversation
	var earlier []Message
	var err error
	if in.ConversationID != "" {
		if conv, err = t.Conversations.FindConversation(ctx, in.ConversationID, in.UserID); err != nil {
			return TutorReply{}, err
		}
		// Read before the new question is stored, so the model is not shown it twice. The latest
		// turns, not the first ones: a long thread must not replay its opening forever.
		if earlier, err = t.Conversations.ListMessages(ctx, conv.ID, HistoryTurns); err != nil {
			return TutorReply{}, err
		}
	} else if conv, err = t.Conversations.CreateConversation(ctx, in.UserID, TitleFrom(question), in.TopicID); err != nil {
		return TutorReply{}, err
	}

	if _, err := t.Conversations.AddMessage(ctx, NewMessage{ConversationID: conv.ID, Role: "learner", Content: question}); err != nil {
		return TutorReply{}, err
	}

	// A follow-up stays on the conversation's topic without the client resending it.
	topicID := in.TopicID
	if topicID == nil {
		topicID = conv.TopicID
	}
	facts, err := t.Conversations.FindTutorFacts(ctx, in.UserID, topicID)
	if err != nil {
		return TutorReply{}, err
	}
	tc := TutorContext{TutorFacts: facts, CurrentQuestion: in.CurrentQuestion}

	var history []Turn
	for _, m := range earlier {
		role := "assistant"
		if m.Role == "learner" {
			role = "user"
		}
		history = append(history, Turn{Role: role, Content: m.Content})
	}

	grounded := false
	if t.Material != nil {
		if grounded, err = t.Material.HasMaterial(ctx, in.UserID); err != nil {
			return TutorReply{}, err
		}
	}
	var reply Answer
	if grounded {
		reply, err = AnswerFromMaterial(ctx, t.AI, t.Material, in.UserID, question, tc, history)
	} else {
		reply, err = AnswerFreely(ctx, t.AI, question, tc, history)
	}
	if err != nil {
		return TutorReply{}, err
	}

	var stored []Citation // NULL for an ungrounded reply
	if reply.Grounding != nil {
		stored = reply.Citations
	}
	// What the conversation is about: as given, or else what the cited excerpts are tagged with.
	if topicID == nil {
		if inferred := TopicOf(reply.Citations); inferred != nil {
			if err := t.Conversations.SetTopic(ctx, conv.ID, *inferred); err != nil {
				return TutorReply{}, err
			}
			topicID = inferred
		}
	}
	var check *QuickCheck
	if reply.Check != nil {
		id, err := t.Conversations.SaveCheck(ctx, in.UserID, *reply.Check)
		if err != nil {
			return TutorReply{}, err
		}
		check = &QuickCheck{QuestionID: id, Body: reply.Check.Body, Options: reply.Check.Options, Citation: reply.CheckCitation}
	}
	answer, err := t.Conversations.AddMessage(ctx, NewMessage{
		ConversationID: conv.ID,
		Role:           "tutor",
		Content:        reply.Text,
		InputTokens:    reply.Usage.InputTokens,
		OutputTokens:   reply.Usage.OutputTokens,
		Model:          reply.Model,
		Citations:      stored,
	})
	if err != nil {
		return TutorReply{}, err
	}

	return TutorReply{
		ConversationID: conv.ID,
		Title:          conv.Title,
		Answer:         reply.Text,
		Grounding:      reply.Grounding,
		Citations:      reply.Citations,
		Messages: []MessageView{
			{Role: "learner", Content: question, CreatedAt: answer.CreatedAt, Citations: []Citation{}},
			toView(answer),
		},
		Usage:   TutorUsage{InputTokens: reply.Usage.InputTokens, OutputTokens: reply.Usage.OutputTokens, Model: reply.Model},
		TopicID: topicID,
		Check:   check,
	}, nil
}

// TopicOf is the topic most of the cited excerpts are tagged with (the earliest cited wins a
// tie), or nil when none is tagged.
func TopicOf(cited []Citation) *string {
	counts := map[string]int{}
	var best *string
	for _, c := range cited {
		if c.TopicID == nil {
			continue
		}
		counts[*c.TopicID]++
		if best == nil || counts[*c.TopicID] > counts[*best] {
			best = c.TopicID
		}
	}
	return best
}

// NoMaterialLabel heads every answer given to a learner with no course material, in the same
// words CheckGrounding uses for outside knowledge, so it is never mistaken for their course.
const NoMaterialLabel = "General knowledge, not from your course material — you have not added any yet. " +
	"Upload your notes, slides or lecture videos on the Sources page and answers will come from them, with citations."

// AnswerFreely answers from general knowledge, for a learner with no course material.
func AnswerFreely(ctx context.Context, c *Client, question string, tc TutorContext, history []Turn) (Answer, error) {
	res, err := c.Text(ctx, Request{
		System: TutorSystem,
		Prompt: TutorPrompt(question, tc),
		// Sonnet-tier: a person reads this and judges it, and the tutor is asked dozens of
		// questions in a session.
		Quality:   Standard,
		MaxTokens: 2000,
		History:   history,
	})
	if err != nil {
		return Answer{}, err
	}
	return Answer{Text: NoMaterialLabel + "\n\n" + jsTrim(res.Text), Citations: []Citation{}, Usage: res.Usage, Model: res.Model}, nil
}

// GroundedReply is the model's raw answer to a grounded question, not yet trusted.
type GroundedReply struct {
	Grounding        string    `json:"grounding"`
	Answer           string    `json:"answer"`
	OutsideKnowledge string    `json:"outsideKnowledge"`
	Check            *RawCheck `json:"check"`
}

// RawCheck is the model's quick check question, not yet trusted: see ValidCheck.
type RawCheck struct {
	Question      string   `json:"question"`
	Options       []string `json:"options"`
	CorrectIndex  int      `json:"correctIndex"`
	Explanation   string   `json:"explanation"`
	SourceExcerpt int      `json:"sourceExcerpt"`
}

// AnswerFromMaterial answers from the learner's course material, with citations that open
// the exact page, slide or second. Exported so the evaluation script runs exactly what the
// tutor runs.
//
// The model labels its own answer source, partial or none, but the label is not trusted on its
// own: see CheckGrounding.
func AnswerFromMaterial(ctx context.Context, c *Client, m Material, userID, question string, tc TutorContext, history []Turn) (Answer, error) {
	query := SearchQuery(ctx, c, question, tc.PreferredLanguage)
	if tc.TopicName != nil && *tc.TopicName != "" {
		if query != "" {
			query += " "
		}
		query += *tc.TopicName
	}
	excerpts, err := m.Retrieve(ctx, userID, query, excerptsPerAnswer)
	if err != nil {
		return Answer{}, err
	}

	shown := make([]PromptExcerpt, len(excerpts))
	for i, e := range excerpts {
		shown[i] = PromptExcerpt{N: e.N, Label: e.SourceTitle + ", " + e.Location, Text: e.Excerpt}
	}
	// send rather than JSON: the stored reply records which model answered, and with
	// server-side fallbacks that is not always the one asked for.
	res, err := c.send(ctx, Request{
		System:    GroundedTutorSystem,
		Prompt:    GroundedTutorPrompt(question, tc, shown),
		Schema:    GroundedAnswerSchema,
		Quality:   Standard,
		MaxTokens: 2500,
		History:   history,
	})
	if err != nil {
		return Answer{}, err
	}
	var raw *GroundedReply
	if err := json.Unmarshal([]byte(res.Text), &raw); err != nil {
		return Answer{}, malformed("The model returned something that is not valid JSON.")
	}

	text, grounding, cited := CheckGrounding(raw, excerpts)
	out := Answer{Text: text, Grounding: &grounding, Citations: cited, Retrieved: excerpts, Usage: res.Usage, Model: res.Model}
	if raw != nil {
		out.Check, out.CheckCitation = ValidCheck(raw.Check, cited)
	}
	return out, nil
}

// ValidCheck keeps a quick check only if it can be trusted to grade a learner: a question, four
// distinct options, a correct index among them, and written from an excerpt the answer actually
// cited that is tagged with a topic (the attempt has to be recorded against something). cited is
// empty for an ungrounded answer, so those never get a check.
func ValidCheck(raw *RawCheck, cited []Citation) (*CheckQuestion, Citation) {
	if raw == nil || jsTrim(raw.Question) == "" || len(raw.Options) != 4 || raw.CorrectIndex < 0 || raw.CorrectIndex >= 4 {
		return nil, Citation{}
	}
	options := make([]string, 4)
	for i, o := range raw.Options {
		options[i] = jsTrim(o)
		if options[i] == "" || slices.Contains(options[:i], options[i]) {
			return nil, Citation{}
		}
	}
	for _, c := range cited {
		if c.N == raw.SourceExcerpt && c.TopicID != nil {
			return &CheckQuestion{TopicID: *c.TopicID, Body: truncate(jsTrim(raw.Question), 1000), Options: options,
				CorrectIndex: raw.CorrectIndex, Explanation: jsTrim(raw.Explanation), ChunkID: c.ChunkID}, c
		}
	}
	return nil, Citation{}
}

// SearchQuery is what to search the learner's (English-indexed) material with. Full-text search
// finds nothing for a question in Devanagari or Hinglish, so such a question is translated to
// English with a small cheap call and searched together with the original (the search ORs its
// words, so both count). A failed translation falls back to the question alone: an answer from
// fewer excerpts beats no answer.
func SearchQuery(ctx context.Context, c *Client, question, preferredLanguage string) string {
	if !NeedsTranslation(question, preferredLanguage) {
		return question
	}
	res, err := c.Text(ctx, Request{System: TranslateSystem, Prompt: TranslatePrompt(question), Quality: Standard, MaxTokens: 200})
	if err != nil {
		slog.Warn("tutor: could not translate the question for search", "err", err)
		return question
	}
	if english := jsTrim(res.Text); english != "" {
		return question + " " + english
	}
	return question
}

// NeedsTranslation: the question is not mostly ASCII (Devanagari, say), or the learner studies in
// Hindi or Hinglish, where even Latin-script words ("kya hota hai") are not English.
func NeedsTranslation(question, preferredLanguage string) bool {
	if preferredLanguage == "hindi" || preferredLanguage == "hinglish" {
		return true
	}
	letters, ascii := 0, 0
	for _, r := range question {
		if unicode.IsLetter(r) {
			letters++
			if r < unicode.MaxASCII {
				ascii++
			}
		}
	}
	return letters > 0 && ascii*10 < letters*8 // under 80% ASCII letters
}

var citationMarker = regexp.MustCompile(`\[(\d+)\]`)

// CheckGrounding applies the honesty rules to a grounded reply. An answer only counts as
// source-backed if it cites an excerpt it was actually given: citation numbers that point at
// nothing are removed, and an "answer" that cites nothing is moved to the general-knowledge
// section, where it is clearly marked as not from the course. A nil reply is not grounded.
func CheckGrounding(reply *GroundedReply, excerpts []Citation) (text string, grounding Grounding, cited []Citation) {
	known := make(map[int]Citation, len(excerpts))
	for _, e := range excerpts {
		known[e.N] = e
	}

	var answer, outside string
	grounding = GroundingNone
	if reply != nil {
		answer = jsTrim(reply.Answer)
		outside = jsTrim(reply.OutsideKnowledge)
		if g := Grounding(reply.Grounding); g == GroundingSource || g == GroundingPartial {
			grounding = g
		}
	}

	// Drop citation markers that point at excerpts the model was never given.
	answer = citationMarker.ReplaceAllStringFunc(answer, func(marker string) string {
		if n, err := strconv.Atoi(marker[1 : len(marker)-1]); err == nil {
			if _, ok := known[n]; ok {
				return marker
			}
		}
		return ""
	})

	var ns []int
	for _, m := range citationMarker.FindAllStringSubmatch(answer, -1) {
		n, _ := strconv.Atoi(m[1])
		if !slices.Contains(ns, n) {
			ns = append(ns, n)
		}
	}
	slices.Sort(ns)
	cited = make([]Citation, 0, len(ns))
	for _, n := range ns {
		cited = append(cited, known[n])
	}

	if grounding != GroundingNone && len(cited) == 0 {
		outside = joinNonEmpty("\n\n", answer, outside)
		answer = ""
		grounding = GroundingNone
	}
	if grounding == GroundingNone {
		// Nothing shown as "from your course" may carry a citation chip.
		answer = ""
		cited = []Citation{}
	}

	first := answer
	if grounding == GroundingNone {
		first = "This is not covered in your course material."
	}
	second := ""
	if outside != "" {
		second = "General knowledge, not from your course material: " + outside
	}
	return joinNonEmpty("\n\n", first, second), grounding, cited
}

func toView(m Message) MessageView {
	c := m.Citations
	if c == nil {
		c = []Citation{}
	}
	return MessageView{Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt, Citations: c}
}

// TitleFrom makes a readable title from the first question, so the history list means
// something at a glance. It cuts at a word boundary: a list of truncated questions is the only
// way a learner finds the thread they want again.
func TitleFrom(question string) string {
	oneLine := []rune(strings.Join(strings.FieldsFunc(question, isJSSpace), " "))
	if len(oneLine) <= 60 {
		return string(oneLine)
	}
	cut := oneLine[:60]
	if i := lastIndexRune(cut, ' '); i > 20 {
		cut = cut[:i]
	}
	return string(cut) + "…"
}

func lastIndexRune(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

func truncate(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n])
	}
	return s
}

func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), sep)
}

// isJSSpace is JavaScript's \s: Unicode White_Space minus U+0085, plus U+FEFF.
func isJSSpace(r rune) bool {
	return r == '\uFEFF' || (r != '\u0085' && unicode.IsSpace(r))
}

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }
