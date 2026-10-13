package assessment

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// The diagnostic establishes what a learner already knows, before any plan exists. Without
// it every topic has the same full gap and the plan's ranking collapses to exam weight alone.
//
// Selection is therefore broad, not deep: cover as many topics as the paper allows, one
// question each. Three questions on Trees say nothing about the eleven topics they displaced.

// Diagnostic paper settings.
const (
	// Fewer than ~10 and a subject with two questions is judged by a coin flip; many more
	// and a learner abandons the thing standing between them and their first plan.
	DiagnosticDefaultQuestions = 15
	DiagnosticMinQuestions     = 6
	DiagnosticMaxQuestions     = 25
	// Without a cap, one deep subject spends the whole paper and the rest report nothing.
	DiagnosticMaxPerSubject = 4
	// One question per topic. Depth is what practice is for.
	DiagnosticMaxPerTopic = 1
	// More generous than practice: rushing a diagnostic gives a pessimistic picture, and that
	// becomes a plan that wastes time on things the learner already knew.
	DiagnosticSecondsPerQuestion = 75
)

// DiagnosticTopic is a topic the diagnostic may ask about.
type DiagnosticTopic struct {
	TopicID     string `json:"topicId"`
	Name        string `json:"name"`
	SubjectID   string `json:"subjectId"`
	SubjectName string `json:"subjectName"`
	// SubjectWeight is the exam importance of the topic's subject.
	SubjectWeight float64 `json:"subjectWeight"`
	// AvailableQuestions is how many verified questions the topic has.
	AvailableQuestions int `json:"availableQuestions"`
}

// DiagnosticPick is one question slot on the paper.
type DiagnosticPick struct {
	TopicID     string `json:"topicId"`
	TopicName   string `json:"topicName"`
	SubjectID   string `json:"subjectId"`
	SubjectName string `json:"subjectName"`
	Position    int    `json:"position"`
}

// SubjectCoverage is how many questions a subject got.
type SubjectCoverage struct {
	SubjectID     string `json:"subjectId"`
	SubjectName   string `json:"subjectName"`
	QuestionCount int    `json:"questionCount"`
}

// UncoveredSubject is a subject that could not be assessed, named rather than silently absent.
type UncoveredSubject struct {
	SubjectName string `json:"subjectName"`
	Reason      string `json:"reason"`
}

// DiagnosticPlan is the chosen paper.
type DiagnosticPlan struct {
	Picks     []DiagnosticPick   `json:"picks"`
	Coverage  []SubjectCoverage  `json:"coverage"`
	Uncovered []UncoveredSubject `json:"uncovered"`
}

// jsRound is JavaScript's Math.round: halves round towards +Infinity, unlike math.Round.
func jsRound(x float64) float64 {
	r := math.Round(x)
	if x-r == 0.5 {
		r++
	}
	return r
}

// PlanDiagnostic chooses the paper.
//
// It round-robins across subjects rather than filling one at a time, so a paper cut short by
// a thin bank still covers breadth, and visits subjects heaviest-first, so when there are more
// subjects than questions the ones that matter most to the exam are the ones measured.
func PlanDiagnostic(topics []DiagnosticTopic, requestedQuestions int) DiagnosticPlan {
	target := min(DiagnosticMaxQuestions, max(DiagnosticMinQuestions, requestedQuestions))

	type subject struct {
		id, name string
		weight   float64
		queue    []DiagnosticTopic
	}
	// Group usable topics by subject, keeping first-seen order (a topic with no verified
	// questions cannot be measured).
	var subjects []*subject
	byID := map[string]*subject{}
	for _, t := range topics {
		if t.AvailableQuestions <= 0 {
			continue
		}
		s := byID[t.SubjectID]
		if s == nil {
			s = &subject{id: t.SubjectID, name: t.SubjectName, weight: t.SubjectWeight}
			byID[t.SubjectID] = s
			subjects = append(subjects, s)
		}
		s.queue = append(s.queue, t)
	}

	// A subject with nothing measurable is named: a breakdown missing a subject the learner
	// cares about reads as a bug rather than as an empty question bank.
	uncovered := []UncoveredSubject{}
	for _, t := range topics {
		if byID[t.SubjectID] != nil || slices.ContainsFunc(uncovered, func(u UncoveredSubject) bool { return u.SubjectName == t.SubjectName }) {
			continue
		}
		uncovered = append(uncovered, UncoveredSubject{SubjectName: t.SubjectName, Reason: "has no questions yet, so it could not be assessed"})
	}

	plan := DiagnosticPlan{Picks: []DiagnosticPick{}, Coverage: []SubjectCoverage{}, Uncovered: uncovered}
	if len(subjects) == 0 {
		return plan
	}

	// Heaviest subject first; within a subject, deepest bank first — most likely to yield.
	slices.SortStableFunc(subjects, func(a, b *subject) int { return cmp.Compare(b.weight, a.weight) })
	for _, s := range subjects {
		slices.SortStableFunc(s.queue, func(a, b DiagnosticTopic) int {
			return cmp.Compare(b.AvailableQuestions, a.AvailableQuestions)
		})
	}

	// Round-robin: each pass takes at most one question per subject, so breadth is established
	// before any subject gets a second question.
	taken := map[string]int{}
	var takenOrder []*subject
	for progressed := true; len(plan.Picks) < target && progressed; {
		progressed = false
		for _, s := range subjects {
			if len(plan.Picks) >= target {
				break
			}
			if taken[s.id] >= DiagnosticMaxPerSubject || len(s.queue) == 0 {
				continue
			}
			t := s.queue[0]
			s.queue = s.queue[1:]
			plan.Picks = append(plan.Picks, DiagnosticPick{
				TopicID: t.TopicID, TopicName: t.Name, SubjectID: s.id, SubjectName: s.name,
				Position: len(plan.Picks) + 1,
			})
			if taken[s.id] == 0 {
				takenOrder = append(takenOrder, s)
			}
			taken[s.id]++
			progressed = true
		}
	}

	for _, s := range takenOrder {
		plan.Coverage = append(plan.Coverage, SubjectCoverage{SubjectID: s.id, SubjectName: s.name, QuestionCount: taken[s.id]})
	}
	return plan
}

// DiagnosticAnswer is one marked question from the sitting.
type DiagnosticAnswer struct {
	TopicID     string `json:"topicId"`
	SubjectID   string `json:"subjectId"`
	SubjectName string `json:"subjectName"`
	IsCorrect   bool   `json:"isCorrect"`
	// Answered is false for a skipped question.
	Answered bool `json:"answered"`
}

// KnowledgeLevel is where a subject sits, in words a learner can act on.
type KnowledgeLevel string

// Knowledge levels.
const (
	LevelStrong     KnowledgeLevel = "strong"
	LevelModerate   KnowledgeLevel = "moderate"
	LevelWeak       KnowledgeLevel = "weak"
	LevelUnmeasured KnowledgeLevel = "unmeasured"
)

// SubjectLevel is one subject's result.
type SubjectLevel struct {
	SubjectID   string         `json:"subjectId"`
	SubjectName string         `json:"subjectName"`
	Correct     int            `json:"correct"`
	Total       int            `json:"total"`
	Percent     float64        `json:"percent"`
	Level       KnowledgeLevel `json:"level"`
}

// DiagnosticResult summarises a sitting.
type DiagnosticResult struct {
	CorrectCount    int     `json:"correctCount"`
	AnsweredCount   int     `json:"answeredCount"`
	TotalQuestions  int     `json:"totalQuestions"`
	AccuracyPercent float64 `json:"accuracyPercent"`
	// Subjects is weakest first — the order the plan will spend time in.
	Subjects []SubjectLevel `json:"subjects"`
	// Verdict is one sentence connecting the result to what happens next.
	Verdict string `json:"verdict"`
}

// Deliberately coarse bands: two to four questions per subject cannot support "63%", and a
// precise number reads as a verdict rather than a starting point. These never write mastery —
// that comes from the recorded attempts, so there is no second number to drift.
const (
	strongAt  = 75
	weakBelow = 45
)

func levelFor(percent float64, total int) KnowledgeLevel {
	switch {
	case total == 0:
		return LevelUnmeasured
	case percent >= strongAt:
		return LevelStrong
	case percent < weakBelow:
		return LevelWeak
	}
	return LevelModerate
}

// percentOf is a percentage to one decimal place, 0 for an empty total.
func percentOf(correct, total int) float64 {
	if total == 0 {
		return 0
	}
	return jsRound(float64(correct)/float64(total)*1000) / 10
}

// SummariseDiagnostic summarises the sitting.
//
// A skipped question counts as wrong for the score — otherwise skipping would raise it — but
// is reported separately, because a half-answered diagnostic is not a measurement.
func SummariseDiagnostic(answers []DiagnosticAnswer) DiagnosticResult {
	res := DiagnosticResult{TotalQuestions: len(answers), Subjects: []SubjectLevel{}}

	index := map[string]int{}
	for _, a := range answers {
		if a.IsCorrect {
			res.CorrectCount++
		}
		if a.Answered {
			res.AnsweredCount++
		}
		i, ok := index[a.SubjectID]
		if !ok {
			i = len(res.Subjects)
			index[a.SubjectID] = i
			res.Subjects = append(res.Subjects, SubjectLevel{SubjectID: a.SubjectID, SubjectName: a.SubjectName})
		}
		res.Subjects[i].Total++
		if a.IsCorrect {
			res.Subjects[i].Correct++
		}
	}
	for i := range res.Subjects {
		s := &res.Subjects[i]
		s.Percent = percentOf(s.Correct, s.Total)
		s.Level = levelFor(s.Percent, s.Total)
	}
	// Weakest first: read as "what your plan will start with".
	slices.SortStableFunc(res.Subjects, func(a, b SubjectLevel) int { return cmp.Compare(a.Percent, b.Percent) })

	res.AccuracyPercent = percentOf(res.CorrectCount, res.TotalQuestions)
	res.Verdict = diagnosticVerdict(res.Subjects, res.AnsweredCount, res.TotalQuestions)
	return res
}

func diagnosticVerdict(subjects []SubjectLevel, answered, total int) string {
	if total == 0 {
		return "There was nothing to assess."
	}
	if answered == 0 {
		return "Nothing was answered, so your plan will start from scratch rather than from this."
	}
	if answered < total {
		skipped := total - answered
		return fmt.Sprintf("You left %d question%s blank. Your plan will treat those topics as unknown, which is safe but means more time on them than you may need.", skipped, plural(skipped))
	}

	var weak []string
	strong := 0
	for _, s := range subjects {
		switch s.Level {
		case LevelWeak:
			weak = append(weak, s.SubjectName)
		case LevelStrong:
			strong++
		}
	}
	if len(weak) > 0 {
		return fmt.Sprintf("Your plan will start with %s. That is where the time is worth spending.", strings.Join(weak[:min(2, len(weak))], " and "))
	}
	if strong == len(subjects) {
		return "Solid across everything measured. Your plan will keep it that way rather than teaching you what you already know."
	}
	return "A reasonable starting point. Your plan will spend the most time where this went least well."
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
