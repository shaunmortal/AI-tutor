package practice

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/ai/validate"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// --- GET /recommendations/next ---

type nextSession struct {
	Recommendation *adaptive.NextBestAction    `json:"recommendation"`
	Questions      []adaptive.PracticeQuestion `json:"questions"`
	BankNote       *string                     `json:"bankNote,omitzero"`
	Reason         string                      `json:"reason,omitzero"`
}

// firstBatch is how many questions to write for a topic nobody has practised: about twenty
// seconds and four AI calls, which is what a learner will wait through on a first visit.
const firstBatch = 3

// nextRecommendation is "what should I do now, and why". Runs as the learner (RLS behind the
// token's user id); the response holds no answers. Question generation, when a topic is empty
// or running dry, uses the elevated client.
func nextRecommendation(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID := auth.UserID(r)
		minutes, subject, ok := parseNextQuery(r)
		if !ok {
			return httpx.E(http.StatusBadRequest, "INVALID_INPUT",
				"minutes must be a whole number between 1 and 600, and subject must be a uuid.")
		}
		ctx := r.Context()

		var plan *adaptive.SessionPlan
		var selection adaptive.SelectedQuestions
		selectAsUser := func() error {
			return d.DB.AsUser(ctx, userID, func(tx pgx.Tx) error {
				var err error
				selection, err = adaptive.SelectQuestions(ctx, store.NewQuestions(tx), adaptive.SelectOptions{
					UserID: userID, TopicID: plan.Candidate.TopicID, Difficulty: plan.Difficulty, Count: plan.RequestedQuestionCount})
				return err
			})
		}

		if err := d.DB.AsUser(ctx, userID, func(tx pgx.Tx) error {
			candidates, err := store.NewTopics(tx).FindCandidatesForLearner(ctx, userID, time.Now(), subject)
			if err != nil {
				return err
			}
			profile, err := store.NewLearners(tx).FindProfile(ctx, userID)
			if err != nil {
				return err
			}
			in := adaptive.RecommendationInput{Candidates: candidates, AvailableMinutes: minutes}
			if profile != nil && profile.AvgSecondsPerQuestion != nil && *profile.AvgSecondsPerQuestion != 0 {
				in.SecondsPerQuestion = profile.AvgSecondsPerQuestion
			}
			plan = adaptive.PlanSession(in)
			return nil
		}); err != nil {
			return err
		}

		if plan == nil {
			// An explainable empty state rather than a 404.
			reason := "No topics are in scope yet. Set a learning goal to get started."
			if subject != "" {
				reason = "That subject is not part of your current goal."
			}
			httpx.OK(w, http.StatusOK, nextSession{Questions: []adaptive.PracticeQuestion{}, Reason: reason})
			return nil
		}

		if err := selectAsUser(); err != nil {
			return err
		}
		topicID, difficulty := plan.Candidate.TopicID, string(plan.Difficulty)

		// A topic with nothing to practise is filled on the way past (an AI-written syllabus is
		// topic names with no questions behind them). Outside any transaction: it takes seconds.
		if len(selection.Questions) == 0 &&
			fillBank(ctx, d, genInput{topicID, difficulty, min(plan.RequestedQuestionCount, firstBatch), userID}) {
			if err := selectAsUser(); err != nil {
				return err
			}
		}
		// Ran out of new questions: this session uses repeats, new ones are written for the next.
		if selection.Repeats > 0 {
			topUp(d, genInput{topicID, difficulty, firstBatch, userID})
		}

		if len(selection.Questions) == 0 {
			httpx.OK(w, http.StatusOK, nextSession{Questions: []adaptive.PracticeQuestion{},
				Reason: plan.Candidate.Name + " has no questions available yet."})
			return nil
		}
		action := adaptive.FinaliseAction(*plan, len(selection.Questions))
		httpx.OK(w, http.StatusOK, map[string]any{
			"recommendation": action, "questions": selection.Questions, "bankNote": selection.Note,
		})
		return nil
	}
}

// parseNextQuery is z.coerce.number().int().min(1).max(600).optional() for minutes and
// z.string().uuid().optional() for subject.
func parseNextQuery(r *http.Request) (minutes *float64, subject string, ok bool) {
	q := r.URL.Query()
	if vals, present := q["minutes"]; present {
		if len(vals) != 1 {
			return nil, "", false
		}
		s := strings.TrimSpace(vals[0])
		f := 0.0 // Number("") is 0
		if s != "" {
			var err error
			if f, err = strconv.ParseFloat(s, 64); err != nil {
				return nil, "", false
			}
		}
		if _, err := intIn("minutes", f, 1, 600); err != nil {
			return nil, "", false
		}
		minutes = &f
	}
	if vals, present := q["subject"]; present {
		if len(vals) != 1 || !isUUID(vals[0]) {
			return nil, "", false
		}
		subject = vals[0]
	}
	return minutes, subject, true
}

// --- the question bank ---

type genInput struct {
	topicID    string
	difficulty string
	count      int
	userID     string // recorded as the author; "" for none
}

type genOutcome struct {
	Body       string `json:"body"`
	Difficulty string `json:"difficulty"`
	Verified   bool   `json:"verified"` // whether a learner will ever see it
	Reason     string `json:"reason"`
	Stored     bool   `json:"stored"`
}

type genResult struct {
	Topic     string              `json:"topic"`
	Requested int                 `json:"requested"`
	Outcomes  []genOutcome        `json:"outcomes"`
	Discarded []map[string]string `json:"discarded"`
	Usage     struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
		Calls        int   `json:"calls"`
	} `json:"usage"`
}

func (g *genResult) anyUsable() bool {
	for _, o := range g.Outcomes {
		if o.Verified && o.Stored {
			return true
		}
	}
	return false
}

// genTimeout bounds one generation run (one authoring call plus one check per question) and
// is also how long the per-topic lock lives, so a crashed run cannot hold a topic forever.
const genTimeout = 10 * time.Minute

var errAIUnavailable = httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", "AI features are not configured on this server.")

// claim takes the per-topic generation lock, so two requests never pay for the same topic at
// once. Fails open on a Redis error: duplicate spend is the only cost of getting this wrong.
// ponytail: release is a plain DEL, so a run outliving genTimeout could free a later claimant's lock.
func claim(ctx context.Context, d *app.Deps, topicID string) (release func(), ok bool) {
	key := "qgen:" + topicID
	got, err := d.Redis.SetNX(ctx, key, "1", genTimeout).Result()
	if err != nil {
		slog.Warn("question generation lock unavailable", "topic", topicID, "err", err)
		return func() {}, true
	}
	if !got {
		return nil, false
	}
	return func() { d.Redis.Del(context.Background(), key) }, true
}

// fillBank writes questions for a topic that has none and says whether practice can proceed.
// Never fails the request: "what should I study" must not become an error because a syllabus
// was thin. Detached from the request's cancellation, so a dropped connection's retry finds
// the work done rather than paying for it twice.
func fillBank(reqCtx context.Context, d *app.Deps, in genInput) bool {
	if d.AI == nil {
		// Worth a log line: a deployment missing its key looks like every new syllabus being empty.
		slog.Warn("could not fill the bank", "topic", in.topicID, "err", errAIUnavailable)
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), genTimeout)
	defer cancel()
	questions := store.NewQuestions(d.DB.Admin)
	verified := func() bool {
		n, err := questions.CountVerifiedForTopic(ctx, in.topicID)
		if err != nil {
			slog.Warn("could not fill the bank", "topic", in.topicID, "err", err)
		}
		return n > 0
	}

	release, ok := claim(ctx, d, in.topicID)
	if !ok {
		// Somebody else is generating this topic: wait for them, then use what they wrote.
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for d.Redis.Exists(ctx, "qgen:"+in.topicID).Val() == 1 {
			select {
			case <-ctx.Done():
				return false
			case <-tick.C:
			}
		}
		return verified()
	}
	defer release()

	// Checked inside the claim: another request may have filled it and released just now.
	if verified() {
		return true
	}
	res, err := generateForTopic(ctx, d, in)
	if err != nil {
		slog.Warn("on-demand generation failed", "topic", in.topicID, "err", err)
		return false
	}
	return res.anyUsable()
}

// topUp writes a few new questions in the background for a topic the learner has run dry.
// Fire-and-forget; skipped when that topic is already being generated.
func topUp(d *app.Deps, in genInput) {
	if d.AI == nil {
		slog.Warn("could not top up topic", "topic", in.topicID, "err", errAIUnavailable)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), genTimeout)
		defer cancel()
		release, ok := claim(ctx, d, in.topicID)
		if !ok {
			return
		}
		defer release()
		if _, err := generateForTopic(ctx, d, in); err != nil {
			slog.Warn("top-up failed", "topic", in.topicID, "err", err)
		}
	}()
}

// generateForTopic grows the bank: generate -> validate -> independently re-solve each
// question without its proposed answer -> store, visible only if the two answers agree.
// Elevated client: the pipeline reads and writes the answer key.
func generateForTopic(ctx context.Context, d *app.Deps, in genInput) (*genResult, error) {
	admin := d.DB.Admin
	questions := store.NewQuestions(admin)
	topic, err := questions.FindTopicWithSubject(ctx, in.topicID)
	if err != nil {
		return nil, err
	}
	if topic == nil {
		return nil, httpx.E(http.StatusNotFound, "TOPIC_NOT_FOUND", "That topic does not exist.")
	}
	existing, err := questions.FindBodiesForTopic(ctx, in.topicID, 50)
	if err != nil {
		return nil, err
	}

	// A topic built from the learner's own material gets questions written from it, each
	// remembering the excerpt it came from.
	var excerpts []store.ChunkRow
	if in.userID != "" {
		if excerpts, err = store.NewSources(admin).ChunksForTopic(ctx, in.topicID, in.userID, 8); err != nil {
			return nil, err
		}
	}

	req := ai.Request{System: ai.QuestionAuthorSystem, Quality: ai.High}
	if len(excerpts) > 0 {
		pe := make([]ai.PromptExcerpt, len(excerpts))
		for i, e := range excerpts {
			pe[i] = ai.PromptExcerpt{N: i + 1, Text: e.Content}
		}
		req.Schema = ai.SourcedQuestionsSchema
		req.Prompt = ai.SourcedQuestionPrompt(ai.SourcedQuestionInput{TopicName: topic.Name,
			Difficulty: ai.Difficulty(in.difficulty), Count: in.count, ExistingQuestions: existing, Excerpts: pe})
	} else {
		req.Schema = ai.GeneratedQuestionsSchema
		req.Prompt = ai.QuestionGenerationPrompt(ai.QuestionGenerationInput{SubjectName: topic.SubjectName,
			TopicName: topic.Name, Difficulty: ai.Difficulty(in.difficulty), Count: in.count, ExistingQuestions: existing})
	}
	var raw json.RawMessage
	usage, err := d.AI.JSON(ctx, req, &raw)
	if err != nil {
		return nil, err
	}

	checked := validate.ValidateGeneratedQuestions(raw)
	excerptOf := excerptsByBody(raw, excerpts)

	res := &genResult{Topic: topic.Name, Requested: in.count, Outcomes: []genOutcome{}, Discarded: []map[string]string{}}
	res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.Calls = usage.InputTokens, usage.OutputTokens, 1
	for _, f := range checked.Rejected {
		res.Discarded = append(res.Discarded, map[string]string{"reason": f.Reason})
	}

	known := map[string]bool{}
	for _, b := range existing {
		known[strings.ToLower(strings.TrimSpace(b))] = true
	}
	for _, q := range checked.Accepted {
		// Verifying costs money; never spend it on a question that cannot be stored.
		if known[strings.ToLower(strings.TrimSpace(q.Body))] {
			res.Outcomes = append(res.Outcomes, genOutcome{Body: q.Body, Difficulty: q.Difficulty,
				Reason: "this question already exists on the topic, so it was not checked"})
			continue
		}
		excerpt := excerptOf[strings.TrimSpace(q.Body)]
		verified, reason, u := checkAnswer(ctx, d.AI, q, excerpt)
		res.Usage.InputTokens += u.InputTokens
		res.Usage.OutputTokens += u.OutputTokens
		res.Usage.Calls++

		g := store.GeneratedQuestion{TopicID: in.topicID, QuestionType: q.QuestionType, Body: q.Body, Options: q.Options,
			CorrectOptionIndexes: q.CorrectOptionIndexes, OptionMisconceptions: q.OptionMisconceptions,
			AcceptedAnswers: q.AcceptedAnswers, Rubric: q.Rubric, Explanation: q.Explanation, Difficulty: q.Difficulty,
			IsVerified: verified}
		if q.NumericAnswer != nil {
			g.NumericValue, g.NumericTolerance = q.NumericAnswer.Value, q.NumericAnswer.Tolerance
		}
		if in.userID != "" {
			g.CreatedBy = &in.userID
		}
		if excerpt != nil {
			g.SourceChunkID = &excerpt.ID
		}
		id, err := questions.InsertGenerated(ctx, g)
		if err != nil {
			return nil, err
		}
		if id == nil {
			reason = "a question with this text already exists on this topic"
		}
		res.Outcomes = append(res.Outcomes, genOutcome{Body: q.Body, Difficulty: q.Difficulty, Verified: verified,
			Reason: reason, Stored: id != nil})
	}
	return res, nil
}

// checkAnswer solves the question in a fresh request that never saw the proposed answer. A
// checker failure is not a crash: the question just stays unverified.
func checkAnswer(ctx context.Context, client *ai.Client, q validate.GeneratedQuestion, excerpt *store.ChunkRow) (bool, string, ai.Usage) {
	if q.Typed() {
		return checkTypedAnswer(ctx, client, q, withExcerpt(excerpt, ai.TypedAnswerCheckPrompt(q.Body)))
	}
	prompt := withExcerpt(excerpt, ai.AnswerCheckPrompt(q.Body, q.Options))
	var raw json.RawMessage
	usage, err := client.JSON(ctx, ai.Request{System: ai.QuestionCheckerSystem, Prompt: prompt,
		Schema: ai.AnswerCheckSchema, MaxTokens: 2000, Quality: ai.High}, &raw)
	if err != nil {
		// The AI being unavailable must not turn into a verified question.
		return false, "the answer could not be independently checked", ai.Usage{}
	}
	check, err := validate.ParseAnswerCheck(raw)
	if err != nil {
		return false, "the answer check came back in an unusable shape", usage
	}
	verified, reason := validate.AgreesWithProposedAnswer(q.CorrectOptionIndexes, check)
	return verified, reason, usage
}

func withExcerpt(excerpt *store.ChunkRow, prompt string) string {
	if excerpt == nil {
		return prompt
	}
	return "Answer according to this course excerpt:\n<excerpt>\n" + excerpt.Content + "\n</excerpt>\n\n" + prompt
}

// checkTypedAnswer is checkAnswer for a numeric or short question: the checker writes its own
// answer cold, which must land within the tolerance, or match an accepted wording or satisfy the
// rubric — the same grader a learner's answer goes through.
func checkTypedAnswer(ctx context.Context, client *ai.Client, q validate.GeneratedQuestion, prompt string) (bool, string, ai.Usage) {
	var raw json.RawMessage
	usage, err := client.JSON(ctx, ai.Request{System: ai.TypedAnswerCheckerSystem, Prompt: prompt,
		Schema: ai.TypedAnswerCheckSchema, MaxTokens: 2000, Quality: ai.High}, &raw)
	if err != nil {
		return false, "the answer could not be independently checked", ai.Usage{}
	}
	check, err := validate.ParseTypedCheck(raw)
	if err != nil {
		return false, "the answer check came back in an unusable shape", usage
	}
	if q.QuestionType == "numeric" {
		verified, reason := validate.AgreesWithNumericAnswer(*q.NumericAnswer, check)
		return verified, reason, usage
	}
	if ok, reason := validate.TrustsTypedCheck(check); !ok {
		return false, reason, usage
	}
	key := assessment.ShortKey{Answers: q.AcceptedAnswers, Rubric: q.Rubric}
	if assessment.MatchesShort(key, check.Answer) {
		return true, "independent check agreed with the answer", usage
	}
	g, err := rubricGrader(ctx, client, q.Body, key, check.Answer)
	if err != nil {
		return false, "the answer could not be independently checked", usage
	}
	if !g.Correct {
		return false, "checker answered " + strconv.Quote(check.Answer) + ", which the rubric does not accept", usage
	}
	return true, "independent answer met the rubric", usage
}

// excerptsByBody reads which excerpt each question cites from the raw reply (validation keeps
// only the fields it knows), keyed by trimmed body.
func excerptsByBody(raw json.RawMessage, excerpts []store.ChunkRow) map[string]*store.ChunkRow {
	out := map[string]*store.ChunkRow{}
	var reply struct {
		Questions any `json:"questions"`
	}
	_ = json.Unmarshal(raw, &reply)
	list, _ := reply.Questions.([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		body, ok := m["body"].(string)
		var n float64
		switch v := m["sourceExcerpt"].(type) {
		case float64:
			n = v
		case string:
			n, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
		}
		if i := int(n); ok && float64(i) == n && i >= 1 && i <= len(excerpts) {
			out[strings.TrimSpace(body)] = &excerpts[i-1]
		}
	}
	return out
}

// --- POST /questions/generate ---

func generateQuestions(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID := auth.UserID(r)
		m, err := readObject(w, r)
		if err != nil {
			return err
		}
		v, present := m["topicId"]
		topicID, err := uuidField("topicId", v, present)
		if err != nil {
			return err
		}
		difficulty, err := enumField(m, "difficulty", "medium", "easy", "medium", "hard")
		if err != nil {
			return err
		}
		count := 3
		if c, err := optInt(m, "count", 1, 5); err != nil {
			return err
		} else if c != nil {
			count = *c
		}
		// The TS resolved the provider before touching the topic, so a missing key wins over a 404.
		if d.AI == nil {
			return errAIUnavailable
		}
		res, err := generateForTopic(r.Context(), d, genInput{topicID, difficulty, count, userID})
		if err != nil {
			return err
		}
		httpx.OK(w, http.StatusCreated, res)
		return nil
	}
}
