package adaptive

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// The recommendation engine ranks every topic in the learner's goal and turns the winner
// into a concrete instruction: this topic, this many questions, at this difficulty, for
// about this long, and here is why.
const (
	// The ranking score is built from two 0-to-1 parts, then multiplied by the topic's own
	// exam weight, so an important topic outranks a trivial one at equal need.
	// How far mastery is from complete. The main driver.
	weightMasteryGap = 0.7
	// How long since the learner touched it. Stops one topic hogging every session.
	weightStaleness = 0.3

	// Days without practice at which staleness counts as maximum.
	stalenessSaturatesAtDays = 14

	// A topic at or above this mastery is considered learned for planning purposes.
	masteryConsideredSolid = 75

	// Finish what you started. An unattempted topic scores maximum on both need and
	// staleness, so without this rule the engine opens a new topic every session and never
	// returns to fix a weak one. While any started topic sits below this mastery, new
	// topics wait. Initial breadth is the diagnostic assessment's job, not the session
	// engine's.
	masteryFloorBeforeNewTopics = 50

	// Revision territory: mastery is solid, but it has been sitting untouched this long.
	reviseAfterIdleDays = 10

	// How many questions each kind of session serves.
	countLearnNew = 3
	countPractice = 5
	countRevise   = 4
	countMiniTest = 10

	// Fallback pace when we do not yet know how fast this learner answers.
	defaultSecondsPerQuestion = 90

	// Never promise a session shorter than this; rounding to zero minutes looks broken.
	minEstimatedMinutes = 2

	// How many runners-up to hand back alongside the recommendation.
	alternativesReturned = 2
)

// LearningAction is the kind of session a topic calls for.
type LearningAction string

// LearningAction values.
const (
	LearnNew LearningAction = "learn_new"
	Practice LearningAction = "practice"
	Revise   LearningAction = "revise"
	MiniTest LearningAction = "mini_test"
)

// TopicCandidate is one topic in the learner's scope, with whatever is known about it.
type TopicCandidate struct {
	TopicID string `json:"topicId"`
	Name    string `json:"name"`
	// The topic's exam importance, straight from topics.weight.
	Weight float64 `json:"weight"`
	// Nil when the learner has never attempted this topic.
	MasteryScore         *float64 `json:"masteryScore"`
	RecentAccuracy       *float64 `json:"recentAccuracy"`
	RecentAttempts       int      `json:"recentAttempts"`
	TotalAttempts        int      `json:"totalAttempts"`
	DaysSinceLastAttempt *float64 `json:"daysSinceLastAttempt"`
}

// RecommendationInput is everything the engine decides from.
type RecommendationInput struct {
	Candidates []TopicCandidate `json:"candidates"`
	// From the learner profile; nil until they have answered enough to measure.
	SecondsPerQuestion *float64 `json:"secondsPerQuestion"`
	// Minutes the learner says they have right now, if they said.
	AvailableMinutes *float64 `json:"availableMinutes"`
}

// RankedTopic is a runner-up offered alongside the recommendation.
type RankedTopic struct {
	TopicID      string   `json:"topicId"`
	Name         string   `json:"name"`
	Score        float64  `json:"score"`
	MasteryScore *float64 `json:"masteryScore"`
}

// TopicRef names a topic.
type TopicRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Signals are the numbers behind a recommendation, for the UI to show.
type Signals struct {
	MasteryScore          *float64 `json:"masteryScore"`
	RecentAccuracyPercent *int     `json:"recentAccuracyPercent"`
	TotalAttempts         int      `json:"totalAttempts"`
	DaysSinceLastAttempt  *int     `json:"daysSinceLastAttempt"`
}

// NextBestAction is the single next best thing for a learner to do.
type NextBestAction struct {
	Action           LearningAction `json:"action"`
	Topic            TopicRef       `json:"topic"`
	Difficulty       Difficulty     `json:"difficulty"`
	QuestionCount    int            `json:"questionCount"`
	EstimatedMinutes int            `json:"estimatedMinutes"`
	// One sentence, assembled from the same numbers the decision used.
	Reason       string        `json:"reason"`
	Signals      Signals       `json:"signals"`
	Alternatives []RankedTopic `json:"alternatives"`
}

// SessionPlan is the decision before the question bank has had its say. Split out because
// the number of questions actually available is only known after the topic and difficulty
// are chosen, and both the time estimate and the sentence depend on it.
type SessionPlan struct {
	Candidate             TopicCandidate `json:"candidate"`
	Action                LearningAction `json:"action"`
	Difficulty            Difficulty     `json:"difficulty"`
	DifficultyAdjustment  Adjustment     `json:"difficultyAdjustment"`
	RecentAccuracyPercent *int           `json:"recentAccuracyPercent"`
	// What the engine would like to serve, before checking the bank.
	RequestedQuestionCount int           `json:"requestedQuestionCount"`
	SecondsPerQuestion     float64       `json:"secondsPerQuestion"`
	Alternatives           []RankedTopic `json:"alternatives"`
}

func clamp01(v float64) float64 { return math.Min(1, math.Max(0, v)) }

// PriorityScore is how badly a topic needs attention. An unattempted topic is treated as a
// full gap, because unknown is not the same as fine.
func PriorityScore(c TopicCandidate) float64 {
	gap := 1.0
	if c.MasteryScore != nil {
		gap = clamp01((100 - *c.MasteryScore) / 100)
	}
	// Never attempted counts as maximally stale: it has been waiting since day one.
	staleness := 1.0
	if c.DaysSinceLastAttempt != nil {
		staleness = clamp01(*c.DaysSinceLastAttempt / stalenessSaturatesAtDays)
	}
	return (weightMasteryGap*gap + weightStaleness*staleness) * c.Weight
}

func orZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func chooseAction(c TopicCandidate) LearningAction {
	if c.TotalAttempts == 0 {
		return LearnNew
	}
	isSolid := orZero(c.MasteryScore) >= masteryConsideredSolid
	isIdle := orZero(c.DaysSinceLastAttempt) >= reviseAfterIdleDays
	if isSolid && isIdle {
		return Revise
	}
	// Solid and recently practised: a short test is more useful than more of the same.
	if isSolid {
		return MiniTest
	}
	return Practice
}

// sentenceFor builds the sentence the learner reads, from the final numbers rather than
// the requested ones: promising five questions and serving one is the kind of small
// dishonesty that makes people stop trusting the recommendation.
func sentenceFor(plan SessionPlan, questionCount int) string {
	c := plan.Candidate
	name := c.Name
	mastery := int(jsRound(orZero(c.MasteryScore)))
	questions := fmt.Sprintf("%d questions", questionCount)
	if questionCount == 1 {
		questions = "1 question"
	}
	pct := "null"
	if plan.RecentAccuracyPercent != nil {
		pct = fmt.Sprint(*plan.RecentAccuracyPercent)
	}

	switch plan.Action {
	case LearnNew:
		return fmt.Sprintf("You have not attempted %s yet, so start with %s at %s level to find where you stand.", name, questions, plan.Difficulty)
	case Revise:
		return fmt.Sprintf("%s is one of your stronger areas but you last practised it %d days ago, so %s will keep it fresh.", name, int(jsRound(orZero(c.DaysSinceLastAttempt))), questions)
	case MiniTest:
		return fmt.Sprintf("%s looks solid at mastery %d, so a short test of %s will confirm it under time pressure.", name, mastery, questions)
	}
	switch plan.DifficultyAdjustment {
	case AdjustSteppedBack:
		return fmt.Sprintf("%s is at mastery %d but your recent accuracy is only %s%%, so %s at %s level will rebuild the basics.", name, mastery, pct, questions, plan.Difficulty)
	case AdjustSteppedUp:
		return fmt.Sprintf("%s is at mastery %d and your recent accuracy is %s%%, so %s at %s level will push you further.", name, mastery, pct, questions, plan.Difficulty)
	}
	return fmt.Sprintf("%s is at mastery %d, so %s at %s level should move it most.", name, mastery, questions, plan.Difficulty)
}

// EligibleCandidates returns the topics allowed to compete this session. While something
// already started is still below the floor, untouched topics are held back; once everything
// in progress is reasonable, the whole scope competes again.
func EligibleCandidates(candidates []TopicCandidate) []TopicCandidate {
	var unfinished []TopicCandidate
	for _, c := range candidates {
		if c.TotalAttempts > 0 && orZero(c.MasteryScore) < masteryFloorBeforeNewTopics {
			unfinished = append(unfinished, c)
		}
	}
	if len(unfinished) > 0 {
		return unfinished
	}
	return candidates
}

// PlanSession is phase one: choose the topic, the kind of session and the difficulty.
// Returns nil when there is nothing in scope.
func PlanSession(in RecommendationInput) *SessionPlan {
	if len(in.Candidates) == 0 {
		return nil
	}

	type scored struct {
		c     TopicCandidate
		score float64
	}
	var ranked []scored
	for _, c := range EligibleCandidates(in.Candidates) {
		ranked = append(ranked, scored{c, PriorityScore(c)})
	}
	// Tie-break on name so the same inputs always produce the same order.
	slices.SortStableFunc(ranked, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), localeCompare(a.c.Name, b.c.Name))
	})

	winner := ranked[0].c
	action := chooseAction(winner)
	dc := ChooseDifficulty(DifficultySignals{
		MasteryScore:   orZero(winner.MasteryScore),
		RecentAccuracy: winner.RecentAccuracy,
		RecentAttempts: winner.RecentAttempts,
	})

	baseCount := countPractice
	switch action {
	case LearnNew:
		baseCount = countLearnNew
	case Revise:
		baseCount = countRevise
	case MiniTest:
		baseCount = countMiniTest
	}

	secondsPerQuestion := float64(defaultSecondsPerQuestion)
	if in.SecondsPerQuestion != nil {
		secondsPerQuestion = *in.SecondsPerQuestion
	}

	// Respect the time the learner actually has. Promising a 15 minute session to somebody
	// with 5 minutes free is how a plan stops being followed.
	requested := baseCount
	if in.AvailableMinutes != nil {
		fit := math.Floor(*in.AvailableMinutes * 60 / secondsPerQuestion)
		requested = int(math.Max(1, math.Min(float64(baseCount), fit)))
	}

	alternatives := []RankedTopic{}
	for _, r := range ranked[1:min(len(ranked), 1+alternativesReturned)] {
		alternatives = append(alternatives, RankedTopic{
			TopicID:      r.c.TopicID,
			Name:         r.c.Name,
			Score:        jsRound(r.score*1000) / 1000,
			MasteryScore: r.c.MasteryScore,
		})
	}

	return &SessionPlan{
		Candidate:              winner,
		Action:                 action,
		Difficulty:             dc.Difficulty,
		DifficultyAdjustment:   dc.Adjustment,
		RecentAccuracyPercent:  dc.RecentAccuracyPercent,
		RequestedQuestionCount: requested,
		SecondsPerQuestion:     secondsPerQuestion,
		Alternatives:           alternatives,
	}
}

// FinaliseAction is phase two: fix the plan to the number of questions the bank could
// actually supply, and write the sentence from those final numbers.
func FinaliseAction(plan SessionPlan, availableQuestionCount int) NextBestAction {
	questionCount := min(plan.RequestedQuestionCount, availableQuestionCount)
	estimatedMinutes := int(math.Max(minEstimatedMinutes,
		jsRound(float64(questionCount)*plan.SecondsPerQuestion/60)))

	c := plan.Candidate
	signals := Signals{MasteryScore: c.MasteryScore, TotalAttempts: c.TotalAttempts}
	if c.RecentAccuracy != nil {
		p := int(jsRound(*c.RecentAccuracy * 100))
		signals.RecentAccuracyPercent = &p
	}
	if c.DaysSinceLastAttempt != nil {
		d := int(jsRound(*c.DaysSinceLastAttempt))
		signals.DaysSinceLastAttempt = &d
	}

	return NextBestAction{
		Action:           plan.Action,
		Topic:            TopicRef{ID: c.TopicID, Name: c.Name},
		Difficulty:       plan.Difficulty,
		QuestionCount:    questionCount,
		EstimatedMinutes: estimatedMinutes,
		Reason:           sentenceFor(plan, questionCount),
		Signals:          signals,
		Alternatives:     plan.Alternatives,
	}
}

// RecommendNextAction runs both phases, for callers that already know the bank can fill
// the request. Returns nil when there is nothing in scope.
func RecommendNextAction(in RecommendationInput) *NextBestAction {
	plan := PlanSession(in)
	if plan == nil {
		return nil
	}
	a := FinaliseAction(*plan, plan.RequestedQuestionCount)
	return &a
}

// localeCompare approximates JavaScript's String.prototype.localeCompare (ICU root
// collation) for the names it is used on: case-insensitive first, then lower case before
// upper case. It does not reproduce ICU's ordering of accents or punctuation.
func localeCompare(a, b string) int {
	return cmp.Or(strings.Compare(strings.ToLower(a), strings.ToLower(b)), strings.Compare(b, a))
}
