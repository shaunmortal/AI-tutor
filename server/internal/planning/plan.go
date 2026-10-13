// Package planning turns a goal and a mastery picture into a day-by-day study plan, and decides
// when that plan has stopped describing reality.
//
// Pure and deterministic, and deliberately not the model: dividing a fixed number of minutes
// between topics by need and exam weight is arithmetic. A model-made plan could not be
// reproduced, unit tested, or explained to a learner who asks why Tuesday looks like that.
//
// The plan algorithm:
//
//  1. budget    days remaining x minutes per day
//  2. need      per topic: how far from mastered, weighted by exam importance
//  3. allocate  split the budget by need, then round to whole sessions with a floor
//  4. lay out   fill day by day, worst-first, without splitting a topic too thin
//
// Proportional allocation alone produces four-minute sessions, which look busy and teach
// nothing — so a session has a floor, and topics that cannot clear it do not appear at all.
package planning

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/shaunmortal/AI-tutor/server/internal/dates"
)

// PlanTopic is a topic in the goal's scope, with what is known about the learner on it.
type PlanTopic struct {
	TopicID     string  `json:"topicId"`
	Name        string  `json:"name"`
	SubjectName *string `json:"subjectName"`
	// Weight is exam importance from the syllabus. Higher means more of the paper.
	Weight float64 `json:"weight"`
	// MasteryScore is 0-100, or nil for a topic never attempted.
	MasteryScore *float64 `json:"masteryScore"`
	Attempts     int      `json:"attempts"`
}

// DueRevision is a topic the spaced-repetition schedule says is owed a review.
type DueRevision struct {
	TopicID     string  `json:"topicId"`
	Name        string  `json:"name"`
	SubjectName *string `json:"subjectName"`
	// DueOn is YYYY-MM-DD, already past or equal to today.
	DueOn string `json:"dueOn"`
	// Lapses is how many times this topic has collapsed back to a one-day interval.
	Lapses             int      `json:"lapses"`
	LastReviewAccuracy *float64 `json:"lastReviewAccuracy"`
}

// PlanInputs is everything BuildPlan needs.
type PlanInputs struct {
	// StartDate is YYYY-MM-DD, the first day the plan schedules work on.
	StartDate string `json:"startDate"`
	// DaysRemaining is whole days from StartDate up to and including the day before the exam.
	DaysRemaining int         `json:"daysRemaining"`
	DailyMinutes  int         `json:"dailyMinutes"`
	Topics        []PlanTopic `json:"topics"`
	// SecondsPerQuestion is from the learner's profile, so question counts match their real pace.
	SecondsPerQuestion *float64 `json:"secondsPerQuestion"`
	// DueRevisions are scheduled before new learning, longest overdue first: a topic past its
	// due date is being actively forgotten, which is worse than one merely not yet learned.
	DueRevisions []DueRevision `json:"dueRevisions,omitempty"`
}

// Session kinds.
const (
	KindLearn  = "learn"
	KindRevise = "revise"
)

// PlannedSession is one block of work on one topic on one day.
type PlannedSession struct {
	ScheduledDate    string `json:"scheduledDate"`
	TopicID          string `json:"topicId"`
	TopicName        string `json:"topicName"`
	SortOrder        int    `json:"sortOrder"`
	PlannedMinutes   int    `json:"plannedMinutes"`
	PlannedQuestions int    `json:"plannedQuestions"`
	Kind             string `json:"kind"` // KindLearn or KindRevise
	Reason           string `json:"reason"`
}

// OmittedTopic is a topic that did not fit, so the caller can say so instead of dropping it.
type OmittedTopic struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// StudyPlan is the result of BuildPlan.
type StudyPlan struct {
	Sessions            []PlannedSession `json:"sessions"`
	TotalMinutesPlanned int              `json:"totalMinutesPlanned"`
	TopicsCovered       int              `json:"topicsCovered"`
	OmittedTopics       []OmittedTopic   `json:"omittedTopics"`
}

// Plan constants. Opinions, kept where they can be argued with.
const (
	// MinSessionMinutes is the shortest block worth scheduling — roughly long enough to read a
	// topic and answer a few questions on it.
	MinSessionMinutes = 15
	// MaxSessionMinutes stops a long day being spent entirely on the weakest topic.
	MaxSessionMinutes = 60
	// MaxTopicsPerDay: more than this is a list, not a plan.
	MaxTopicsPerDay = 4
	// TopicsPerDayTarget is how many topics a day should hold when long enough. The session cap
	// alone did not spread anything at a 60-minute day; two half-hour blocks teach more than
	// four fifteen-minute ones and still show a four-topic plan inside two days.
	TopicsPerDayTarget = 2
	// FallbackSecondsPerQuestion is the assumed pace with no measured average yet.
	FallbackSecondsPerQuestion = 90
	// MasteryTarget is treated as needing no further work. Not 100: the last points come from
	// revision, and pushing 85 to 100 takes time from a topic sitting at 20.
	MasteryTarget = 85
	// MaxRevisionShareOfDay is how much of a day revision may take before new learning — enough
	// to clear a backlog in a few days while the plan still visibly progresses.
	MaxRevisionShareOfDay = 0.5
	// RevisionMinutes is shorter than a learning session on purpose: refresh, not re-teach.
	RevisionMinutes = 15
	// UnknownMastery is assumed for a never-attempted topic — the mastery formula's neutral prior.
	// Treating unknown as 0 made an unmeasured topic outrank one just measured as weak, so a wrong
	// diagnostic answer lowered that topic's share. Unknown (gap 0.59) now sits between measured
	// weak (0.66) and measured solid (0.15).
	UnknownMastery = 35
	// MinNeedFactor keeps a well-known topic alive instead of abandoning it before the exam.
	MinNeedFactor = 0.15
)

// jsRound is JavaScript's Math.round: halves round up, not away from zero.
func jsRound(x float64) int { return int(math.Floor(x + 0.5)) }

// TopicNeed is how much attention a topic needs before the budget is known: the gap below
// MasteryTarget (floored at MinNeedFactor) times exam weight (floored at 0.1).
func TopicNeed(topic PlanTopic) float64 {
	mastery := float64(UnknownMastery)
	if topic.MasteryScore != nil {
		mastery = *topic.MasteryScore
	}
	rawGap := math.Max(0, MasteryTarget-mastery) / MasteryTarget
	gap := math.Max(MinNeedFactor, rawGap)
	return gap * math.Max(0.1, topic.Weight)
}

// BuildPlan builds the plan. It returns an empty plan rather than failing when there is nothing
// to plan — no topics in scope, or an exam already past — since a learner can genuinely be there.
func BuildPlan(inputs PlanInputs) StudyPlan {
	empty := StudyPlan{Sessions: []PlannedSession{}, OmittedTopics: []OmittedTopic{}}

	if inputs.DaysRemaining <= 0 || len(inputs.Topics) == 0 {
		return empty
	}

	secondsPerQuestion := float64(FallbackSecondsPerQuestion)
	if s := inputs.SecondsPerQuestion; s != nil && *s > 0 {
		secondsPerQuestion = *s
	}
	questionsFor := func(minutes int) int {
		return max(1, jsRound(float64(minutes*60)/secondsPerQuestion))
	}

	// ---- 1. the budget, and the ceiling on how much of it a day can hold ----
	perDayCapacity := min(inputs.DailyMinutes, MaxTopicsPerDay*MaxSessionMinutes)
	totalBudget := inputs.DaysRemaining * perDayCapacity
	if perDayCapacity <= 0 {
		// The DB CHECK keeps this >= 10. The TS produced NaN minutes here; Go would divide by zero.
		return empty
	}

	// A goal may set as little as ten minutes a day, and a fifteen-minute floor cannot fit in it:
	// every such learner got an empty "not enough time" plan with 180 days left. Somebody who says
	// ten minutes a day means it.
	sessionFloor := min(MinSessionMinutes, perDayCapacity)

	// ---- 2. need per topic, worst first ----
	type rankedTopic struct {
		topic PlanTopic
		need  float64
	}
	ranked := make([]rankedTopic, len(inputs.Topics))
	totalNeed := 0.0
	for i, topic := range inputs.Topics {
		ranked[i] = rankedTopic{topic, TopicNeed(topic)}
	}
	slices.SortStableFunc(ranked, func(a, b rankedTopic) int { return cmp.Compare(b.need, a.need) })
	for _, r := range ranked {
		totalNeed += r.need
	}
	if totalNeed <= 0 {
		return empty
	}

	// ---- 3. allocate, and decide who does not fit ----
	omitted := []OmittedTopic{}
	type queue struct {
		topic     PlanTopic
		remaining int
	}
	var queues []*queue
	remainingBudget := totalBudget

	for _, r := range ranked {
		share := jsRound(r.need / totalNeed * float64(totalBudget))

		// Whole sessions only, so the layout never splits a four-minute remainder across days.
		sessions := min(share, remainingBudget) / sessionFloor
		if sessions < 1 {
			omitted = append(omitted, OmittedTopic{
				Name:   r.topic.Name,
				Reason: fmt.Sprintf("not enough time before the exam to give it a useful %d minutes", sessionFloor),
			})
			continue
		}

		minutes := sessions * sessionFloor
		queues = append(queues, &queue{r.topic, minutes})
		remainingBudget -= minutes
	}

	if len(queues) == 0 {
		empty.OmittedTopics = omitted
		return empty
	}

	// ---- 4. lay out across days ----
	// Round-robin, so a learner meets their whole plan in the first week. The per-day cap is
	// usually the day divided by how many topics it should hold — what actually makes it spread.
	sessionCap := min(MaxSessionMinutes, max(sessionFloor, perDayCapacity/TopicsPerDayTarget))

	sessions := []PlannedSession{}

	// Revision first, on the earliest days: the learning pass fills each day to capacity, so
	// anything not reserved in advance would be crowded out and a backlog would never appear.
	revisionByDay := map[string]int{}

	// (day, topic) slots revision took. A due topic is also in scope, and scheduling it twice on
	// one day violates study_sessions_unique_slot and failed the whole plan insert.
	type slot struct{ date, topicID string }
	claimed := map[slot]bool{}

	for index, due := range inputs.DueRevisions {
		minutes := min(RevisionMinutes, perDayCapacity)

		// Spread across days: a ten-topic backlog is not a ten-topic day.
		perDayBudget := int(math.Floor(float64(perDayCapacity) * MaxRevisionShareOfDay))
		if perDayBudget < minutes {
			break
		}

		slotsPerDay := max(1, perDayBudget/minutes)
		dayOffset := index / slotsPerDay
		if dayOffset >= inputs.DaysRemaining {
			break
		}

		date := dates.AddDays(inputs.StartDate, dayOffset)
		sessions = append(sessions, PlannedSession{
			ScheduledDate:    date,
			TopicID:          due.TopicID,
			TopicName:        due.Name,
			SortOrder:        index % slotsPerDay,
			PlannedMinutes:   minutes,
			PlannedQuestions: questionsFor(minutes),
			Kind:             KindRevise,
			Reason:           revisionReason(due),
		})
		revisionByDay[date] += minutes
		claimed[slot{date, due.TopicID}] = true
	}

	anyRemaining := func() bool {
		return slices.ContainsFunc(queues, func(q *queue) bool { return q.remaining > 0 })
	}

	for dayIndex := 0; dayIndex < inputs.DaysRemaining && anyRemaining(); dayIndex++ {
		date := dates.AddDays(inputs.StartDate, dayIndex)

		// Subtract what revision claimed, or the day is planned to twice its capacity.
		takenByRevision, revised := revisionByDay[date]
		dayLeft := perDayCapacity - takenByRevision
		placedToday := 0
		if revised {
			placedToday = 1
		}

		// Rotate which topic starts the day. Without it a day with room for one topic gave it to
		// the same queue until empty — ten minutes a day meant 25 days on one topic. Rotation
		// changes only the order, never the totals.
		start := dayIndex % len(queues)

		for step := range queues {
			q := queues[(start+step)%len(queues)]

			if q.remaining <= 0 {
				continue
			}
			if placedToday >= MaxTopicsPerDay || dayLeft < sessionFloor {
				break
			}
			// Revision covered this topic today. Skipped, not merged: the minutes stay queued for
			// a later day, so the topic keeps its full allocation.
			if claimed[slot{date, q.topic.TopicID}] {
				continue
			}

			minutes := min(q.remaining, dayLeft, sessionCap)

			// Never leave a stub too small to be its own session: fold it in when the day has room.
			folded := minutes
			if stub := q.remaining - minutes; stub > 0 && stub < sessionFloor && dayLeft-minutes >= stub {
				folded += stub
			}

			sessions = append(sessions, PlannedSession{
				ScheduledDate:    date,
				TopicID:          q.topic.TopicID,
				TopicName:        q.topic.Name,
				SortOrder:        placedToday,
				PlannedMinutes:   folded,
				PlannedQuestions: questionsFor(folded),
				Kind:             KindLearn,
				Reason:           sessionReason(q.topic),
			})

			q.remaining -= folded
			dayLeft -= folded
			placedToday++
		}
	}

	// Still queued means out of days rather than out of budget: the goal's daily minutes do not
	// add up to its exam date.
	for _, q := range queues {
		if q.remaining > 0 {
			omitted = append(omitted, OmittedTopic{
				Name:   q.topic.Name,
				Reason: fmt.Sprintf("%d minutes of this could not be fitted before the exam", q.remaining),
			})
		}
	}

	total := 0
	covered := map[string]bool{}
	for _, s := range sessions {
		total += s.PlannedMinutes
		covered[s.TopicID] = true
	}

	return StudyPlan{
		Sessions:            sessions,
		TotalMinutesPlanned: total,
		TopicsCovered:       len(covered),
		OmittedTopics:       omitted,
	}
}

func subjectSuffix(subjectName *string) string {
	if subjectName == nil || *subjectName == "" {
		return ""
	}
	return " (" + *subjectName + ")"
}

// revisionReason says why a revision is due, in words.
func revisionReason(due DueRevision) string {
	subject := subjectSuffix(due.SubjectName)

	if due.Lapses > 1 {
		return fmt.Sprintf("%s%s has slipped %d times — a short review to hold onto it.", due.Name, subject, due.Lapses)
	}
	if due.LastReviewAccuracy != nil && *due.LastReviewAccuracy < 60 {
		return fmt.Sprintf("You scored %d%% on %s%s last time, so it is due again.", jsRound(*due.LastReviewAccuracy), due.Name, subject)
	}
	return fmt.Sprintf("%s%s is due for revision before you forget it.", due.Name, subject)
}

// sessionReason says why this topic is in the plan, in words a learner can argue with.
func sessionReason(topic PlanTopic) string {
	subject := subjectSuffix(topic.SubjectName)

	if topic.Attempts == 0 {
		return fmt.Sprintf("%s%s has not been started yet.", topic.Name, subject)
	}

	mastery := 0
	if topic.MasteryScore != nil {
		mastery = jsRound(*topic.MasteryScore)
	}

	switch {
	case mastery < 40:
		return fmt.Sprintf("%s%s is at mastery %d from %d attempts, so it needs the most time.", topic.Name, subject, mastery, topic.Attempts)
	case mastery < MasteryTarget:
		return fmt.Sprintf("%s%s is at mastery %d and close to solid — this should finish it.", topic.Name, subject, mastery)
	default:
		return fmt.Sprintf("%s%s is at mastery %d; this is to keep it from fading.", topic.Name, subject, mastery)
	}
}
