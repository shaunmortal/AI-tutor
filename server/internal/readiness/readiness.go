// Package readiness scores exam readiness: one number, and the arithmetic behind it.
//
// A readiness score nobody can interrogate is a horoscope, so every component is returned
// alongside the total, with the weight it carried and a sentence naming what would move it.
// No model is called: arithmetic a learner can check by hand is worth more here than a
// fluent paragraph they cannot.
package readiness

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Topic is a topic in the goal's scope, with what the learner has demonstrated on it.
type Topic struct {
	TopicID     string  `json:"topicId"`
	Name        string  `json:"name"`
	SubjectName *string `json:"subjectName"`
	// Exam importance from the syllabus. Higher means more of the paper.
	Weight float64 `json:"weight"`
	// 0-100, or nil for a topic never attempted.
	MasteryScore *float64 `json:"masteryScore"`
	Attempts     int      `json:"attempts"`
}

// Mock is a mock test that was actually finished. Open tests tell us nothing yet.
type Mock struct {
	ScorePercent float64 `json:"scorePercent"`
	// ISO 8601 timestamp; ordered as a string, so keep one format.
	SubmittedAt string `json:"submittedAt"`
}

// Input is everything readiness is scored from.
type Input struct {
	Topics []Topic `json:"topics"`
	Mocks  []Mock  `json:"mocks"`
	// Whole days from today to the exam. Zero or negative means the exam is here.
	DaysRemaining int `json:"daysRemaining"`
	DailyMinutes  int `json:"dailyMinutes"`
}

// Band is the readiness band. Confidence is how much the score should be trusted, which is
// a different question from what it is.
type (
	Band       string
	Confidence string
)

// Band and Confidence values.
const (
	NotReady Band = "not_ready"
	Building Band = "building"
	OnTrack  Band = "on_track"
	Ready    Band = "ready"

	Low    Confidence = "low"
	Medium Confidence = "medium"
	High   Confidence = "high"
)

// Component is one part of the score.
type Component struct {
	Key   string `json:"key"` // mastery, coverage, mock or time
	Label string `json:"label"`
	// 0-100.
	Score float64 `json:"score"`
	// Share of the total this component actually carried, after any omissions.
	Weight float64 `json:"weight"`
	Detail string  `json:"detail"`
}

// Gap is a topic costing readiness: heavy on the paper and far from mastered.
type Gap struct {
	TopicID      string   `json:"topicId"`
	Name         string   `json:"name"`
	SubjectName  *string  `json:"subjectName"`
	MasteryScore *float64 `json:"masteryScore"`
	// Weight × distance from target. The ranking key, exposed so the order is checkable.
	Cost float64 `json:"cost"`
}

// Result is the score and everything behind it.
type Result struct {
	// 0-100.
	Score      float64    `json:"score"`
	Band       Band       `json:"band"`
	Confidence Confidence `json:"confidence"`
	// One sentence a learner can act on.
	Verdict    string      `json:"verdict"`
	Components []Component `json:"components"`
	// Topics answered and still short of the target, worst first. At most maxGaps.
	Gaps []Gap `json:"gaps"`
	// Heaviest topics not attempted at all. Kept apart from Gaps because their cost assumes
	// a mastery of zero rather than measuring one. At most maxGaps.
	NotStarted      []Gap `json:"notStarted"`
	DaysRemaining   int   `json:"daysRemaining"`
	TopicsInScope   int   `json:"topicsInScope"`
	TopicsAttempted int   `json:"topicsAttempted"`
	TopicsMastered  int   `json:"topicsMastered"`
	MocksTaken      int   `json:"mocksTaken"`
}

// Band thresholds on the final score, and the mastery a topic is treated as finished at
// (matches the planner's own target). Exported because the screen shows them.
const (
	ReadyAt       = 75
	OnTrackAt     = 55
	BuildingAt    = 30
	MasteryTarget = 85
)

const (
	// Only the most recent few mocks count — a paper from six weeks ago is not current form.
	mockLookback = 3
	// Rough minutes to move one weighted mastery point. Only the time component uses this.
	minutesPerMasteryPoint = 1.2
	// Below this many attempts in total, the score is a first impression, not a measurement.
	thinEvidenceAttempts = 25
	// At or above this, and with a mock sat, the score has enough behind it to lean on.
	solidEvidenceAttempts = 120
	maxGaps               = 5
)

// Mastery carries the most because it is the only component measured question by question.
// Coverage is deliberately smaller: having touched every topic once is a much weaker claim
// than having got them right.
var weights = map[string]float64{"mastery": 0.45, "coverage": 0.2, "mock": 0.25, "time": 0.1}

func clamp(v float64) float64 { return math.Max(0, math.Min(100, v)) }

func round(v float64, places int) float64 {
	factor := math.Pow(10, float64(places))
	return jsRound(v*factor) / factor
}

// num formats a number the way a JavaScript template literal does.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// masteryOf scores a never-attempted topic as zero — the opposite of the planner, which
// gives it a neutral prior because its question is where the next hour should go.
// Readiness asks what the learner has been shown to be able to do, and silence is not
// evidence: scoring an untouched syllabus as half-ready tells somebody they are on track
// for an exam they have not started studying for.
func masteryOf(t Topic) float64 {
	if t.MasteryScore == nil {
		return 0
	}
	return *t.MasteryScore
}

type part struct {
	score  float64
	detail string
}

func totalWeight(topics []Topic) float64 {
	sum := 0.0
	for _, t := range topics {
		sum += t.Weight
	}
	return sum
}

// masteryComponent is weighted average mastery, against the target rather than 100: a
// paper averaging the target is ready, not 85% ready.
func masteryComponent(topics []Topic) *part {
	total := totalWeight(topics)
	if total == 0 {
		return &part{0, "No topics in scope yet."}
	}
	sum := 0.0
	for _, t := range topics {
		sum += t.Weight * masteryOf(t)
	}
	weighted := sum / total
	return &part{
		clamp(weighted / MasteryTarget * 100),
		fmt.Sprintf("Weighted mastery across the syllabus is %s against a target of %d.", num(round(weighted, 1)), MasteryTarget),
	}
}

// coverageComponent is the share of the exam, by weight, attempted at all.
func coverageComponent(topics []Topic) *part {
	total := totalWeight(topics)
	if total == 0 {
		return &part{0, "No topics in scope yet."}
	}
	touchedWeight, touched := 0.0, 0
	for _, t := range topics {
		if t.Attempts > 0 {
			touchedWeight += t.Weight
			touched++
		}
	}
	detail := "Every topic in the syllabus has been attempted."
	if untouched := len(topics) - touched; untouched != 0 {
		detail = fmt.Sprintf("%d of %d topics have not been attempted yet.", untouched, len(topics))
	}
	return &part{clamp(touchedWeight / total * 100), detail}
}

// mockComponent is recent mock form, or nil when no paper has been finished. Sorted here
// rather than trusting the caller: "recent" must not mean whatever order the database
// returned.
func mockComponent(mocks []Mock) *part {
	if len(mocks) == 0 {
		return nil
	}
	recent := slices.Clone(mocks)
	slices.SortStableFunc(recent, func(a, b Mock) int { return strings.Compare(b.SubmittedAt, a.SubmittedAt) })
	recent = recent[:min(len(recent), mockLookback)]

	sum := 0.0
	for _, m := range recent {
		sum += m.ScorePercent
	}
	average := sum / float64(len(recent))

	detail := fmt.Sprintf("Average of the last %d mock tests: %s%%.", len(recent), num(round(average, 1)))
	if len(recent) == 1 {
		detail = fmt.Sprintf("One mock test, scored %s%%.", num(round(average, 1)))
	}
	return &part{clamp(average), detail}
}

// timeComponent asks whether the time left is enough to close the remaining gap at the
// current daily budget.
//
// Omitted when the exam has arrived or no budget is set — somebody sitting the exam
// tomorrow is as ready as their answers say, and time has stopped being something they can
// change. Also omitted when nothing is in scope: an empty syllabus has a gap of zero, and
// time would then be the only surviving component, scoring a learner with no goal as ready.
func timeComponent(topics []Topic, daysRemaining, dailyMinutes int) *part {
	if len(topics) == 0 || daysRemaining <= 0 || dailyMinutes <= 0 {
		return nil
	}
	gap := 0.0
	for _, t := range topics {
		gap += t.Weight * math.Max(0, MasteryTarget-masteryOf(t))
	}
	available := float64(daysRemaining * dailyMinutes)

	// Nothing left to close: time is not a constraint, so it should not drag the score down.
	if gap == 0 {
		return &part{100, "Every topic is already at the target."}
	}
	needed := gap * minutesPerMasteryPoint
	return &part{
		clamp(available / needed * 100),
		fmt.Sprintf("Closing the remaining gap needs roughly %sh; %d days at %d min gives about %sh.",
			num(jsRound(needed/60)), daysRemaining, dailyMinutes, num(jsRound(available/60))),
	}
}

func bandFor(score float64) Band {
	switch {
	case score >= ReadyAt:
		return Ready
	case score >= OnTrackAt:
		return OnTrack
	case score >= BuildingAt:
		return Building
	}
	return NotReady
}

func confidenceFor(totalAttempts, mocksTaken int) Confidence {
	if totalAttempts < thinEvidenceAttempts {
		return Low
	}
	if totalAttempts >= solidEvidenceAttempts && mocksTaken > 0 {
		return High
	}
	return Medium
}

func toGap(t Topic) Gap {
	return Gap{
		TopicID:      t.TopicID,
		Name:         t.Name,
		SubjectName:  t.SubjectName,
		MasteryScore: t.MasteryScore,
		Cost:         round(t.Weight*math.Max(0, MasteryTarget-masteryOf(t)), 2),
	}
}

// gapsWhere ranks the matching topics by cost, worst first, ties alphabetical.
func gapsWhere(topics []Topic, keep func(Topic) bool) []Gap {
	gaps := []Gap{}
	for _, t := range topics {
		if keep(t) {
			gaps = append(gaps, toGap(t))
		}
	}
	slices.SortStableFunc(gaps, func(a, b Gap) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), localeCompare(a.Name, b.Name))
	})
	return gaps[:min(len(gaps), maxGaps)]
}

// verdictFor names the thing most worth doing next, derived from the weakest component
// rather than the total: "you are 48% ready" tells a learner nothing they can act on.
func verdictFor(band Band, components []Component, gaps, notStarted []Gap, confidence Confidence, daysRemaining int) string {
	if len(components) == 0 {
		return "Set a goal and answer a few questions, and this will start to mean something."
	}

	if confidence == Low {
		// Measured weakness first; otherwise the heaviest thing not started. A learner this
		// early usually has nothing measured at all, so falling back matters.
		first := append(slices.Clone(gaps), notStarted...)
		if len(first) > 0 {
			return fmt.Sprintf("Too early to call — there is not enough answered yet to measure this. Practising %s would be a useful start.", first[0].Name)
		}
		return "Too early to call — there is not enough answered yet to measure this."
	}

	weakest := components[0]
	for _, c := range components[1:] {
		if c.Score < weakest.Score {
			weakest = c
		}
	}
	days := ""
	if daysRemaining > 0 {
		days = fmt.Sprintf(" with %d days left", daysRemaining)
	}

	lead := map[Band]string{
		Ready:    "On course" + days + ".",
		OnTrack:  "Broadly on track" + days + ".",
		Building: "Coming along, but not there yet" + days + ".",
		NotReady: "Not ready yet" + days + ".",
	}[band]

	var push string
	switch weakest.Key {
	case "mastery":
		push = "Accuracy is what is holding it back most."
		if len(gaps) > 0 {
			push = fmt.Sprintf("Accuracy is what is holding it back most — %s is costing the most.", gaps[0].Name)
		}
	case "coverage":
		push = "The biggest gain is starting the topics that have not been touched at all."
		if len(notStarted) > 0 {
			push = fmt.Sprintf("The biggest gain is starting the topics not touched at all — %s is the heaviest of them.", notStarted[0].Name)
		}
	case "mock":
		push = "Mock scores are the weakest part — sitting a full paper is the fastest way to move this."
	case "time":
		push = "Time is the binding constraint; raising the daily budget matters more than any single topic now."
	}
	return lead + " " + push
}

// Score scores exam readiness from what the learner has actually done.
//
// Components that cannot be measured are dropped and the rest reweighted, never scored as
// zero. A learner who has not sat a mock is not less ready than one who sat one and failed
// it, and a scheme that says otherwise rewards avoiding the measurement.
func Score(in Input) Result {
	topics := in.Topics

	components := []Component{}
	add := func(key, label string, p *part) {
		if p != nil {
			components = append(components, Component{key, label, round(p.score, 1), weights[key], p.detail})
		}
	}
	if len(topics) > 0 {
		add("mastery", "Mastery", masteryComponent(topics))
		add("coverage", "Syllabus covered", coverageComponent(topics))
	}
	add("mock", "Mock tests", mockComponent(in.Mocks))
	add("time", "Time available", timeComponent(topics, in.DaysRemaining, in.DailyMinutes))

	// Reweight over the components that survived, so the total is always out of 100.
	presentWeight, weightedSum := 0.0, 0.0
	for _, c := range components {
		presentWeight += c.Weight
		weightedSum += c.Score * c.Weight
	}
	score := 0.0
	if presentWeight != 0 {
		score = round(weightedSum/presentWeight, 1)
	}
	for i := range components {
		components[i].Weight = 0
		if presentWeight != 0 {
			components[i].Weight = round(weights[components[i].Key]/presentWeight, 3)
		}
	}

	totalAttempts, attempted, mastered := 0, 0, 0
	for _, t := range topics {
		totalAttempts += t.Attempts
		if t.Attempts > 0 {
			attempted++
		}
		if masteryOf(t) >= MasteryTarget {
			mastered++
		}
	}
	confidence := confidenceFor(totalAttempts, len(in.Mocks))
	band := bandFor(score)

	// Measured gaps leave out never-attempted topics even though those cost the score more:
	// their cost is an assumption, not a measurement, and ranked together the assumptions
	// won and their order came down to spelling. They are reported apart in notStarted,
	// where alphabetical order within one weight is honest — any five of an equally weighted
	// set are a fair answer to "these are heavy and untouched".
	gaps := gapsWhere(topics, func(t Topic) bool { return t.Attempts > 0 && masteryOf(t) < MasteryTarget })
	notStarted := gapsWhere(topics, func(t Topic) bool { return t.Attempts == 0 })

	return Result{
		Score:           score,
		Band:            band,
		Confidence:      confidence,
		Verdict:         verdictFor(band, components, gaps, notStarted, confidence, in.DaysRemaining),
		Components:      components,
		Gaps:            gaps,
		NotStarted:      notStarted,
		DaysRemaining:   in.DaysRemaining,
		TopicsInScope:   len(topics),
		TopicsAttempted: attempted,
		TopicsMastered:  mastered,
		MocksTaken:      len(in.Mocks),
	}
}

// jsRound matches JavaScript's Math.round, which rounds halves towards +Inf (Go's
// math.Round rounds them away from zero).
func jsRound(x float64) float64 {
	f := math.Floor(x)
	if x-f >= 0.5 {
		return f + 1
	}
	return f
}

// localeCompare approximates JavaScript's localeCompare (ICU root collation) for names:
// case-insensitive first, then lower case before upper case. It does not reproduce ICU's
// ordering of accents or punctuation.
func localeCompare(a, b string) int {
	return cmp.Or(strings.Compare(strings.ToLower(a), strings.ToLower(b)), strings.Compare(b, a))
}
