package learner

import (
	"math"

	"github.com/shaunmortal/AI-tutor/server/internal/dates"
)

// Spaced repetition: when a topic should be seen again.
//
// The interval arithmetic is SM-2 (SuperMemo's algorithm, the one Anki uses): a review
// that goes well multiplies the gap by an ease factor, one that goes badly collapses it to
// a day, and ease itself drifts so a topic this learner finds hard returns sooner. Two
// adaptations from the flashcard version:
//
// The unit is a topic, not a card. SM-2 takes a self-reported 0-5 grade; here the grade is
// the measured accuracy across the review's questions, which is strictly better evidence
// because people are famously bad at judging what they know.
//
// The exam caps the interval. Anki assumes you want to remember forever; here there is a
// date after which none of it matters, so intervals are clamped to land before the exam,
// and a topic that cannot fit another review is reported finished rather than scheduled
// into the void.
const (
	// Accuracy at or above which a review passes and the interval grows. 80 rather than
	// 100 because a topic is many questions: insisting on perfection would keep competent
	// topics in daily rotation and crowd out the ones actually at risk.
	PassAccuracyPercent = 80.0

	// Below this the review fails and the interval collapses to one day. Between this and
	// the pass mark is the shaky middle: the interval grows a little, not by the full ease.
	LapseAccuracyPercent = 50.0

	// Growth applied in the shaky middle, instead of the ease factor.
	ShakyMultiplier = 1.2

	// Ease moves by this much on a pass or a lapse, roughly SM-2's step.
	EaseStep = 0.15

	// SM-2's floor: below it the interval barely grows and the schedule is the wrong tool.
	MinEase = 1.3
	MaxEase = 3.0

	// First gap after learning a topic; short because the first forgetting is fastest.
	FirstIntervalDays = 1

	// Second gap, before ease starts multiplying. The standard SM-2 opening.
	SecondIntervalDays = 3

	// Longest gap regardless of exam date. Past this a topic is parked, not revised.
	MaxIntervalDays = 60

	// Answers needed before a review's accuracy may move the schedule a full step. One
	// answer is a coin flip; below this the review counts but takes the cautious path.
	MinQuestionsForFullCredit = 3

	// Days before the exam within which no review is scheduled: a review due the morning
	// of the exam is not a plan.
	ExamBufferDays = 1

	startingEase = 2.5
)

// ReviewState is what the schedule currently says about a topic.
type ReviewState struct {
	IntervalDays int     `json:"intervalDays"`
	EaseFactor   float64 `json:"easeFactor"`
	ReviewCount  int     `json:"reviewCount"`
	Lapses       int     `json:"lapses"`
	// YYYY-MM-DD, or nil before the first review.
	LastReviewedOn *string `json:"lastReviewedOn"`
}

// ReviewOutcome is the result of one review.
type ReviewOutcome struct {
	// YYYY-MM-DD the review happened on.
	ReviewedOn string `json:"reviewedOn"`
	// 0-100 across the questions answered in this review.
	AccuracyPercent float64 `json:"accuracyPercent"`
	// How many questions that accuracy is based on.
	QuestionsAnswered int `json:"questionsAnswered"`
	// Whole days from ReviewedOn to the exam, so the next interval cannot land after it.
	// Nil when there is no goal, in which case only MaxIntervalDays applies.
	DaysUntilExam *int `json:"daysUntilExam"`
}

// ScheduleUpdate is the schedule to store after a first study or a review.
type ScheduleUpdate struct {
	// YYYY-MM-DD.
	DueOn              string  `json:"dueOn"`
	IntervalDays       int     `json:"intervalDays"`
	EaseFactor         float64 `json:"easeFactor"`
	ReviewCount        int     `json:"reviewCount"`
	Lapses             int     `json:"lapses"`
	LastReviewedOn     string  `json:"lastReviewedOn"`
	LastReviewAccuracy float64 `json:"lastReviewAccuracy"`
	// True when this review was a failure and the interval collapsed.
	Lapsed bool `json:"lapsed"`
	// True when no further review fits before the exam.
	FinishedForThisGoal bool `json:"finishedForThisGoal"`
}

// roomBeforeExam is the days a review may be scheduled within; +Inf with no exam.
func roomBeforeExam(daysUntilExam *int) float64 {
	if daysUntilExam == nil {
		return math.Inf(1)
	}
	return float64(*daysUntilExam - ExamBufferDays)
}

// FirstSchedule is the schedule for a topic just studied for the first time. Separate
// from NextReview because a first sighting is not a review: there is no interval to grow
// and no ease to adjust.
func FirstSchedule(studiedOn string, daysUntilExam *int) ScheduleUpdate {
	u := ScheduleUpdate{
		IntervalDays:   FirstIntervalDays,
		EaseFactor:     startingEase,
		LastReviewedOn: studiedOn,
	}
	if roomBeforeExam(daysUntilExam) < FirstIntervalDays {
		u.DueOn = studiedOn
		u.FinishedForThisGoal = true
		return u
	}
	u.DueOn = dates.AddDays(studiedOn, FirstIntervalDays)
	return u
}

// NextReview is the schedule after a review. state is nil for a topic reviewed without a
// schedule yet, which is every topic practised before this feature existed, so it is a
// normal path rather than an edge case.
func NextReview(state *ReviewState, outcome ReviewOutcome) ScheduleUpdate {
	current := ReviewState{IntervalDays: FirstIntervalDays, EaseFactor: startingEase}
	if state != nil {
		current = *state
	}

	accuracy := clamp(outcome.AccuracyPercent, 0, 100)
	thinEvidence := outcome.QuestionsAnswered < MinQuestionsForFullCredit

	reviewCount := current.ReviewCount + 1
	easeFactor := current.EaseFactor
	lapses := current.Lapses
	lapsed := false
	var intervalDays int

	switch {
	case accuracy < LapseAccuracyPercent:
		// Failure: back to tomorrow, and the topic is harder than assumed. Ease falls even
		// on thin evidence; getting one of two wrong is still getting it wrong.
		intervalDays = FirstIntervalDays
		easeFactor = clamp(current.EaseFactor-EaseStep, MinEase, MaxEase)
		lapses++
		lapsed = true
	case accuracy >= PassAccuracyPercent:
		// Pass. Fixed first and second gaps, ease only from the third review, by which
		// point there is enough evidence for a multiplier to mean something.
		switch {
		case thinEvidence:
			// Two right out of two is encouraging, not proof. Step, but do not compound.
			intervalDays = int(jsRound(float64(current.IntervalDays) * ShakyMultiplier))
		case reviewCount == 1:
			intervalDays = SecondIntervalDays
		default:
			intervalDays = int(jsRound(float64(current.IntervalDays) * current.EaseFactor))
			easeFactor = clamp(current.EaseFactor+EaseStep, MinEase, MaxEase)
		}
	default:
		// The shaky middle: some of it stuck. Grow a little, leave ease alone.
		intervalDays = int(jsRound(float64(current.IntervalDays) * ShakyMultiplier))
	}

	intervalDays = min(max(1, intervalDays), MaxIntervalDays)

	// The exam ceiling: everything above is Anki's arithmetic, which assumes remembering
	// forever. This is the part that knows about the exam.
	u := ScheduleUpdate{
		DueOn:              outcome.ReviewedOn,
		IntervalDays:       intervalDays,
		EaseFactor:         round2(easeFactor),
		ReviewCount:        reviewCount,
		Lapses:             lapses,
		LastReviewedOn:     outcome.ReviewedOn,
		LastReviewAccuracy: round2(accuracy),
		Lapsed:             lapsed,
	}
	room := roomBeforeExam(outcome.DaysUntilExam)
	if room < 1 {
		// No time for another review. Reported finished rather than scheduled past the
		// exam, where it would sit in the list forever looking overdue.
		u.FinishedForThisGoal = true
		return u
	}

	// Pulled in to fit rather than dropped: a shortened final review beats none.
	if room < float64(intervalDays) {
		u.IntervalDays = int(room)
	}
	u.DueOn = dates.AddDays(outcome.ReviewedOn, u.IntervalDays)
	return u
}

// DaysOverdue is how overdue a topic is, in days; negative means not yet due, and 0 for
// an unparseable date. The longest overdue is closest to being forgotten, so this orders
// the revision list.
func DaysOverdue(dueOn, today string) int {
	days, ok := dates.DaysBetween(dueOn, today)
	if !ok {
		return 0
	}
	return days
}

// IsDue reports whether a topic is owed a review as of today.
func IsDue(dueOn, today string) bool {
	return DaysOverdue(dueOn, today) >= 0
}
