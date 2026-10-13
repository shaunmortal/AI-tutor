package planning

import (
	"fmt"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/dates"
)

// Re-planning decides whether a plan has stopped describing reality. Two things make a plan
// wrong: the learner did not do it, or they did and it is not working. Both are visible in
// recorded data; neither needs the model.
//
// Re-planning is triggered by opening the plan, so without a rate limit a learner checking five
// times on a bad morning gets five versions, each moving work they have not had a chance to do.
// A plan that changes every time you look at it is not adaptive, it is unusable.

// ReconciledSession is a session old enough that whether it happened is settled.
type ReconciledSession struct {
	ScheduledDate    string `json:"scheduledDate"`
	TopicID          string `json:"topicId"`
	PlannedQuestions int    `json:"plannedQuestions"`
	// QuestionsAnswered is answers actually recorded on this topic on this date.
	QuestionsAnswered int `json:"questionsAnswered"`
	QuestionsCorrect  int `json:"questionsCorrect"`
}

// SessionStatus is what happened in a past session.
type SessionStatus string

// Session statuses.
const (
	StatusCompleted SessionStatus = "completed"
	StatusPartial   SessionStatus = "partial"
	StatusMissed    SessionStatus = "missed"
)

// PastSession is a reconciled session with its status.
type PastSession struct {
	ReconciledSession
	Status SessionStatus `json:"status"`
}

// TopicPerformance is how the learner is doing on a topic recently.
type TopicPerformance struct {
	TopicID      string   `json:"topicId"`
	Name         string   `json:"name"`
	MasteryScore *float64 `json:"masteryScore"`
	// RecentAccuracyPercent is 0-100 over recent answers on this topic.
	RecentAccuracyPercent *float64 `json:"recentAccuracyPercent"`
	RecentAttempts        int      `json:"recentAttempts"`
}

// ReplanInputs is everything ShouldReplan needs.
type ReplanInputs struct {
	// Today is YYYY-MM-DD in the learner's timezone.
	Today string `json:"today"`
	// PlanCreatedAt is when the current plan was built. Zero means unknown, which (like an
	// unparseable timestamp in the TS) does not rate-limit.
	PlanCreatedAt time.Time `json:"planCreatedAt"`
	// Now is passed in so this stays pure and testable.
	Now time.Time `json:"now"`
	// PastSessions are sessions whose date has passed, in any order.
	PastSessions []PastSession      `json:"pastSessions"`
	Topics       []TopicPerformance `json:"topics"`
}

// ReplanReason says which signal triggered a rebuild.
type ReplanReason string

// Replan reasons. The empty reason means no rebuild (null in the TS).
const (
	ReasonMissedSessions  ReplanReason = "missed_sessions"
	ReasonPoorPerformance ReplanReason = "poor_performance"
)

// ReplanVerdict is whether to rebuild, and why.
type ReplanVerdict struct {
	Replan bool         `json:"replan"`
	Reason ReplanReason `json:"reason"`
	// Explanation is what to tell the learner. Empty when there is nothing to say.
	Explanation string `json:"explanation"`
}

// Replan thresholds. Opinions, not facts, kept where they can be argued with.
const (
	// MinMissedToReplan: one miss is a bad day, and rebuilding for it would move remaining work
	// earlier exactly when the learner is behind. Three inside a week is a pattern.
	MinMissedToReplan = 3
	// MissedWindowDays is how far back to look for missed sessions.
	MissedWindowDays = 7
	// PoorAccuracyPercent is deliberately low: half wrong on a hard topic is learning, three
	// quarters wrong is stuck.
	PoorAccuracyPercent = 35
	// MinAttemptsForPoorSignal is answers needed before accuracy is signal rather than noise.
	MinAttemptsForPoorSignal = 4
	// MinStrugglingTopics is how many topics must struggle before the whole plan is rebuilt.
	MinStrugglingTopics = 2
	// MinHoursBetweenReplans is twenty rather than twenty-four so a learner who studies each
	// morning is not blocked by having replanned slightly earlier the previous day.
	MinHoursBetweenReplans = 20
)

// ReconcileStatus says what happened in a session whose date has passed, from recorded attempts
// rather than self-marking. Partial is the common case and not a failure: folding it into
// missed would fire rebuilds at people who are studying.
func ReconcileStatus(session ReconciledSession) SessionStatus {
	if session.QuestionsAnswered <= 0 {
		return StatusMissed
	}
	if session.QuestionsAnswered >= session.PlannedQuestions {
		return StatusCompleted
	}
	return StatusPartial
}

// ShouldReplan says whether this plan should be rebuilt, and why.
//
// The rate limit is checked first: the signals are still true immediately after a rebuild, so
// checking them first would rebuild on every read. Missed sessions beat poor performance because
// "you did not do this" is a fact, while "this is not working" is an inference.
func ShouldReplan(inputs ReplanInputs) ReplanVerdict {
	nothing := ReplanVerdict{}

	if !inputs.PlanCreatedAt.IsZero() && inputs.Now.Sub(inputs.PlanCreatedAt).Hours() < MinHoursBetweenReplans {
		return nothing
	}

	// ---- missed sessions ----
	recentlyMissed := 0
	for _, s := range inputs.PastSessions {
		if s.Status != StatusMissed {
			continue
		}
		if age, ok := dates.DaysBetween(s.ScheduledDate, inputs.Today); ok && age >= 0 && age <= MissedWindowDays {
			recentlyMissed++
		}
	}

	if recentlyMissed >= MinMissedToReplan {
		return ReplanVerdict{
			Replan:      true,
			Reason:      ReasonMissedSessions,
			Explanation: fmt.Sprintf("You missed %d sessions in the last %d days, so the remaining work has been spread over the time you have left.", recentlyMissed, MissedWindowDays),
		}
	}

	// ---- topics the plan is not fixing ----
	var struggling []string
	for _, t := range inputs.Topics {
		if t.RecentAttempts >= MinAttemptsForPoorSignal && t.RecentAccuracyPercent != nil && *t.RecentAccuracyPercent < PoorAccuracyPercent {
			struggling = append(struggling, t.Name)
		}
	}

	if len(struggling) >= MinStrugglingTopics {
		named := strings.Join(struggling[:min(3, len(struggling))], ", ")
		return ReplanVerdict{
			Replan:      true,
			Reason:      ReasonPoorPerformance,
			Explanation: fmt.Sprintf("You are still getting most questions wrong on %s, so those topics have been given more of your remaining time.", named),
		}
	}

	return nothing
}
