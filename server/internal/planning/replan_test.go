package planning

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Re-planning runs when the plan is read, which makes restraint the thing worth testing hardest:
// what triggers a rebuild, and much more importantly, what must not.

const today = "2026-08-14"

var (
	yesterday = time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	now       = time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
)

func past(date string, status SessionStatus, answered int) PastSession {
	return PastSession{
		ReconciledSession: ReconciledSession{
			ScheduledDate:     date,
			TopicID:           "topic-" + date,
			PlannedQuestions:  5,
			QuestionsAnswered: answered,
			QuestionsCorrect:  answered / 2,
		},
		Status: status,
	}
}

func replanInputs() ReplanInputs {
	return ReplanInputs{
		Today:         today,
		PlanCreatedAt: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
		Now:           now,
	}
}

func perf(id, name string, mastery, accuracy float64, attempts int) TopicPerformance {
	return TopicPerformance{TopicID: id, Name: name, MasteryScore: &mastery, RecentAccuracyPercent: &accuracy, RecentAttempts: attempts}
}

func threeMissed() []PastSession {
	return []PastSession{past("2026-08-11", StatusMissed, 0), past("2026-08-12", StatusMissed, 0), past("2026-08-13", StatusMissed, 0)}
}

func twoStruggling() []TopicPerformance {
	return []TopicPerformance{perf("a", "Arrays", 12, 10, 10), perf("b", "Trees", 15, 15, 8)}
}

func TestReconcileStatus(t *testing.T) {
	cases := []struct {
		name              string
		answered, correct int
		want              SessionStatus
	}{
		{"no answers is missed", 0, 0, StatusMissed},
		{"met its target is completed", 5, 3, StatusCompleted},
		{"more than asked is completed, not an error", 9, 7, StatusCompleted},
		// Somebody who answered three of five turned up.
		{"started but not finished is partial", 3, 1, StatusPartial},
	}
	for _, c := range cases {
		got := ReconcileStatus(ReconciledSession{ScheduledDate: today, TopicID: "t", PlannedQuestions: 5, QuestionsAnswered: c.answered, QuestionsCorrect: c.correct})
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestShouldReplanRestraint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ReplanInputs)
		want   bool
	}{
		{"does nothing when the plan is being followed", func(in *ReplanInputs) {
			in.PastSessions = []PastSession{past("2026-08-11", StatusCompleted, 5), past("2026-08-12", StatusCompleted, 5), past("2026-08-13", StatusPartial, 3)}
		}, false},
		// The most important case: without the rate limit, five refreshes make five versions.
		{"refuses to rebuild a plan that was just built, however bad the signal", func(in *ReplanInputs) {
			in.PlanCreatedAt = now
			in.PastSessions = append(threeMissed(), past("2026-08-10", StatusMissed, 0))
			in.Topics = twoStruggling()
		}, false},
		{"allows a rebuild once the previous plan is a day old", func(in *ReplanInputs) {
			in.PlanCreatedAt = yesterday
			in.PastSessions = threeMissed()
		}, true},
		// One missed session is a life, not a pattern.
		{"does not rebuild for a single missed day", func(in *ReplanInputs) {
			in.PastSessions = []PastSession{past("2026-08-13", StatusMissed, 0)}
		}, false},
		// One bad week in June must not rebuild the plan every day until the exam.
		{"ignores sessions missed longer ago than the window", func(in *ReplanInputs) {
			in.PastSessions = []PastSession{past("2026-06-01", StatusMissed, 0), past("2026-06-02", StatusMissed, 0), past("2026-06-03", StatusMissed, 0), past("2026-06-04", StatusMissed, 0)}
		}, false},
		{"does not rebuild over one struggling topic", func(in *ReplanInputs) {
			in.Topics = []TopicPerformance{perf("a", "Arrays", 12, 10, 10)}
		}, false},
		// Two answers is not evidence.
		{"does not treat a couple of wrong answers as a struggling topic", func(in *ReplanInputs) {
			in.Topics = []TopicPerformance{perf("a", "Arrays", 20, 0, 2), perf("b", "Trees", 20, 0, 1)}
		}, false},
	}
	for _, c := range cases {
		in := replanInputs()
		c.mutate(&in)
		v := ShouldReplan(in)
		if v.Replan != c.want {
			t.Errorf("%s: replan = %v, want %v", c.name, v.Replan, c.want)
		}
		if !c.want && v.Reason != "" {
			t.Errorf("%s: reason = %q, want none", c.name, v.Reason)
		}
	}
}

func TestShouldReplanActs(t *testing.T) {
	t.Run("rebuilds after enough missed sessions, and says how many", func(t *testing.T) {
		in := replanInputs()
		in.PastSessions = append(threeMissed(), past("2026-08-10", StatusCompleted, 5))
		v := ShouldReplan(in)
		if !v.Replan || v.Reason != ReasonMissedSessions {
			t.Fatalf("got %+v", v)
		}
		if !strings.Contains(v.Explanation, "3") {
			t.Errorf("explanation %q lacks the count", v.Explanation)
		}
		// Framed as the plan adapting, not as the learner failing.
		if !regexp.MustCompile(`(?i)spread over the time you have left`).MatchString(v.Explanation) {
			t.Errorf("explanation %q", v.Explanation)
		}
	})

	t.Run("rebuilds when several topics are not improving, and names them", func(t *testing.T) {
		in := replanInputs()
		in.Topics = []TopicPerformance{perf("a", "Normalisation", 16, 10, 12), perf("b", "Time Complexity", 22, 25, 8)}
		v := ShouldReplan(in)
		if !v.Replan || v.Reason != ReasonPoorPerformance {
			t.Fatalf("got %+v", v)
		}
		for _, name := range []string{"Normalisation", "Time Complexity"} {
			if !strings.Contains(v.Explanation, name) {
				t.Errorf("explanation %q lacks %s", v.Explanation, name)
			}
		}
	})

	t.Run("reports missed sessions ahead of poor performance", func(t *testing.T) {
		in := replanInputs()
		in.PastSessions = threeMissed()
		in.Topics = twoStruggling()
		if v := ShouldReplan(in); v.Reason != ReasonMissedSessions {
			t.Errorf("reason = %q", v.Reason)
		}
	})

	t.Run("never names more than three topics in one explanation", func(t *testing.T) {
		in := replanInputs()
		for i := range 8 {
			in.Topics = append(in.Topics, perf(fmt.Sprintf("t%d", i), fmt.Sprintf("Topic %d", i), 10, 10, 6))
		}
		v := ShouldReplan(in)
		if !v.Replan {
			t.Fatal("no replan")
		}
		if n := len(regexp.MustCompile(`Topic \d`).FindAllString(v.Explanation, -1)); n != 3 {
			t.Errorf("named %d topics, want 3", n)
		}
	})

	t.Run("holds its thresholds where they can be argued with", func(t *testing.T) {
		if MinMissedToReplan != 3 || MissedWindowDays != 7 || MinStrugglingTopics != 2 || !(MinHoursBetweenReplans < 24) {
			t.Error("thresholds changed")
		}
	})
}
