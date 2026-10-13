package assessment

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
)

// A mock test's topics come from the study plan, not the whole syllabus: a test of things the
// learner was never asked to study yet is demoralising and useless. A test of what the plan has
// scheduled answers the real question — is the studying working?

// Mock test settings.
const (
	// Long enough to be a test, short enough to sit in one go on a phone.
	MockDefaultQuestions = 15
	MockMinQuestions     = 5
	MockMaxQuestions     = 30
	// Fifteen topics is one question each: a breakdown of pure noise. Six is enough to feel
	// like a paper and leave two or three questions each.
	MockMaxTopics = 6
	// Fewest questions a topic can contribute and still appear in the breakdown.
	MockMinPerTopic = 2
	// Used when the learner has no measured pace. More generous than practice: a test has no
	// pause after each answer, and a countdown that runs out early turns it into a stress test.
	MockFallbackSecondsPerQuestion = 100
	// Score at or above which the test counts as passed.
	MockPassPercent = 60
)

// MockTestTopic is a planned topic the test may draw from.
type MockTestTopic struct {
	TopicID     string  `json:"topicId"`
	Name        string  `json:"name"`
	SubjectName *string `json:"subjectName"`
	// Weight is exam importance from the syllabus.
	Weight float64 `json:"weight"`
	// MasteryScore is 0-100, or nil for a topic never attempted.
	MasteryScore *float64 `json:"masteryScore"`
	// PlannedSessions is how many sessions the plan has scheduled on the topic.
	PlannedSessions int `json:"plannedSessions"`
	// AvailableQuestions is the verified bank size; an allocation cannot exceed it.
	AvailableQuestions int `json:"availableQuestions"`
}

// TopicAllocation is how many questions a topic gets, and why it is in the test.
type TopicAllocation struct {
	TopicID       string  `json:"topicId"`
	Name          string  `json:"name"`
	SubjectName   *string `json:"subjectName"`
	QuestionCount int     `json:"questionCount"`
	Reason        string  `json:"reason"`
}

// OmittedTopic is a topic that could not be included, named rather than dropped in silence.
type OmittedTopic struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// MockTestPlan is the assembled test.
type MockTestPlan struct {
	Allocations     []TopicAllocation `json:"allocations"`
	TotalQuestions  int               `json:"totalQuestions"`
	DurationMinutes int               `json:"durationMinutes"`
	Omitted         []OmittedTopic    `json:"omitted"`
}

// TopicShare is how much of the test a topic deserves: planned × gap × weight. Planned is what
// makes it a test of the plan; gap gives weak topics more room. A topic never attempted counts
// as a full gap — zero mastery would imply a measurement that was never taken.
func TopicShare(t MockTestTopic) float64 {
	mastery := 0.0
	if t.MasteryScore != nil {
		mastery = *t.MasteryScore
	}
	gap := math.Max(0.2, (100-mastery)/100)
	planned := 1 + float64(min(3, t.PlannedSessions))*0.5
	return gap * planned * math.Max(0.2, t.Weight)
}

// PlanMockTest builds the test. secondsPerQuestion is the learner's measured pace, or 0 when
// there is none.
//
// It returns an empty plan rather than an error when nothing can be assembled: no planned
// topics or a bank too thin to fill one are states a learner can be in, not faults.
func PlanMockTest(topics []MockTestTopic, requestedQuestions int, secondsPerQuestion float64) MockTestPlan {
	target := min(MockMaxQuestions, max(MockMinQuestions, requestedQuestions))
	plan := MockTestPlan{Allocations: []TopicAllocation{}, Omitted: []OmittedTopic{}}

	type ranked struct {
		topic MockTestTopic
		share float64
		count int
	}
	var usable []ranked
	for _, t := range topics {
		if t.AvailableQuestions >= MockMinPerTopic {
			usable = append(usable, ranked{topic: t, share: TopicShare(t)})
			continue
		}
		reason := "has no questions yet"
		if t.AvailableQuestions != 0 {
			reason = fmt.Sprintf("has only %d question%s, too few to test", t.AvailableQuestions, plural(t.AvailableQuestions))
		}
		plan.Omitted = append(plan.Omitted, OmittedTopic{Name: t.Name, Reason: reason})
	}
	if len(usable) == 0 {
		return plan
	}

	// Highest share first, then capped: the topics that matter most make the cut.
	slices.SortStableFunc(usable, func(a, b ranked) int { return cmp.Compare(b.share, a.share) })
	chosen := usable[:min(MockMaxTopics, len(usable))]
	for _, r := range usable[len(chosen):] {
		plan.Omitted = append(plan.Omitted, OmittedTopic{Name: r.topic.Name, Reason: "left out to keep the test short enough to be worth sitting"})
	}

	totalShare := 0.0
	for _, r := range chosen {
		totalShare += r.share
	}

	// Proportional split, then repair: rounding never lands on the target and each topic has
	// its own floor and ceiling, so allocate roughly and fix the difference one at a time.
	total := 0
	for i := range chosen {
		ideal := int(jsRound(chosen[i].share / totalShare * float64(target)))
		if ideal == 0 {
			ideal = MockMinPerTopic
		}
		chosen[i].count = max(MockMinPerTopic, min(chosen[i].topic.AvailableQuestions, ideal))
		total += chosen[i].count
	}

	// Too many: take from the largest allocation that can spare one (first wins a tie).
	for total > target {
		donor := -1
		for i, r := range chosen {
			if r.count > MockMinPerTopic && (donor < 0 || r.count > chosen[donor].count) {
				donor = i
			}
		}
		if donor < 0 {
			break
		}
		chosen[donor].count--
		total--
	}

	// Too few: give to the highest-share topic with questions left in the bank. If none has,
	// a shorter test is the honest outcome, not a failure.
	for total < target {
		taker := -1
		for i, r := range chosen {
			if r.count < r.topic.AvailableQuestions && (taker < 0 || r.share > chosen[taker].share) {
				taker = i
			}
		}
		if taker < 0 {
			break
		}
		chosen[taker].count++
		total++
	}

	pace := float64(MockFallbackSecondsPerQuestion)
	if secondsPerQuestion > 0 {
		pace = math.Max(secondsPerQuestion, 45)
	}

	for _, r := range chosen {
		plan.Allocations = append(plan.Allocations, TopicAllocation{
			TopicID: r.topic.TopicID, Name: r.topic.Name, SubjectName: r.topic.SubjectName,
			QuestionCount: r.count, Reason: allocationReason(r.topic),
		})
	}
	plan.TotalQuestions = total
	plan.DurationMinutes = int(math.Max(1, jsRound(float64(total)*pace/60)))
	return plan
}

func allocationReason(t MockTestTopic) string {
	if t.MasteryScore == nil {
		return t.Name + " is scheduled in your plan and untested so far."
	}
	m := int(jsRound(*t.MasteryScore))
	switch {
	case m < 40:
		return fmt.Sprintf("%s is at mastery %d — this checks whether the practice is landing.", t.Name, m)
	case m < 75:
		return fmt.Sprintf("%s is at mastery %d and worth confirming under test conditions.", t.Name, m)
	}
	return fmt.Sprintf("%s is at mastery %d; a few questions to prove it holds.", t.Name, m)
}

// MarkedQuestion is one marked question from a sitting.
type MarkedQuestion struct {
	// TopicID is empty for a question with no topic.
	TopicID   string `json:"topicId"`
	IsCorrect bool   `json:"isCorrect"`
	// Answered is false for a skipped question, which is not the same as getting it wrong.
	Answered bool `json:"answered"`
}

// TopicScore is one topic's share of the result.
type TopicScore struct {
	TopicID string  `json:"topicId"`
	Correct int     `json:"correct"`
	Total   int     `json:"total"`
	Percent float64 `json:"percent"`
}

// MockTestResult is a marked sitting.
type MockTestResult struct {
	CorrectCount   int     `json:"correctCount"`
	AnsweredCount  int     `json:"answeredCount"`
	TotalQuestions int     `json:"totalQuestions"`
	ScorePercent   float64 `json:"scorePercent"`
	Passed         bool    `json:"passed"`
	// ByTopic is weakest first, so the learner knows where to go next.
	ByTopic []TopicScore `json:"byTopic"`
	// Verdict is the one thing worth doing off the back of this test.
	Verdict string `json:"verdict"`
}

// MarkMockTest marks the sitting. topicNames maps topic id to name, for the verdict.
//
// A skipped question scores zero, or leaving questions blank would raise the percentage. But
// AnsweredCount is reported separately: "40% having answered everything" and "40% having
// answered half" need different advice.
func MarkMockTest(questions []MarkedQuestion, topicNames map[string]string) MockTestResult {
	res := MockTestResult{TotalQuestions: len(questions), ByTopic: []TopicScore{}}

	index := map[string]int{}
	for _, q := range questions {
		if q.IsCorrect {
			res.CorrectCount++
		}
		if q.Answered {
			res.AnsweredCount++
		}
		if q.TopicID == "" {
			continue
		}
		i, ok := index[q.TopicID]
		if !ok {
			i = len(res.ByTopic)
			index[q.TopicID] = i
			res.ByTopic = append(res.ByTopic, TopicScore{TopicID: q.TopicID})
		}
		res.ByTopic[i].Total++
		if q.IsCorrect {
			res.ByTopic[i].Correct++
		}
	}
	for i := range res.ByTopic {
		res.ByTopic[i].Percent = percentOf(res.ByTopic[i].Correct, res.ByTopic[i].Total)
	}
	// Weakest first: the list reads as a to-do.
	slices.SortStableFunc(res.ByTopic, func(a, b TopicScore) int { return cmp.Compare(a.Percent, b.Percent) })

	res.ScorePercent = percentOf(res.CorrectCount, res.TotalQuestions)
	res.Passed = res.ScorePercent >= MockPassPercent
	res.Verdict = mockVerdict(res.ScorePercent, res.AnsweredCount, res.TotalQuestions, res.ByTopic, topicNames)
	return res
}

func mockVerdict(score float64, answered, total int, byTopic []TopicScore, topicNames map[string]string) string {
	if total == 0 {
		return "There was nothing to mark."
	}
	if answered == 0 {
		return "Nothing was answered, so there is nothing to read into this one."
	}
	if answered < total {
		skipped := total - answered
		return fmt.Sprintf("You left %d question%s blank, so the score is lower than what you know. Worth sitting again with time to finish.", skipped, plural(skipped))
	}
	if score >= 85 {
		return "Strong across the board. Your plan is working — keep going."
	}
	if len(byTopic) > 0 {
		weakest := byTopic[0]
		if name := topicNames[weakest.TopicID]; name != "" && weakest.Percent < 50 {
			return fmt.Sprintf("%s is where this went wrong — %s%% of it. Start there.", name, strconv.FormatFloat(weakest.Percent, 'f', -1, 64))
		}
	}
	if score >= MockPassPercent {
		return "A pass, with room to tighten up. The breakdown below shows where."
	}
	return "Below where you want to be before the exam. The weakest topic is at the top of the list."
}
