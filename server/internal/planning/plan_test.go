package planning

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/shaunmortal/AI-tutor/server/internal/dates"
)

// A plan is a promise about somebody's remaining time before an exam, so the things worth testing
// are the ones that would make it a lie: a day holding more minutes than the learner has, a
// session too short to teach anything, work after the exam, or a weak topic getting less time
// than a strong one.

func ptr[T any](v T) *T { return &v }

// Stable ids without randomness, so a failing test fails the same way twice (same hash as the TS).
func hash(text string) int32 {
	var v int32
	for _, c := range text {
		v = v*31 + c
	}
	return v
}

// topic mirrors the TS fixture: subject "Subject", and mastery nil means never attempted.
func topic(name string, mastery *float64, attempts int, weight float64) PlanTopic {
	return PlanTopic{
		TopicID:      fmt.Sprintf("t-%d", int64(math.Abs(float64(hash(name))))),
		Name:         name,
		SubjectName:  ptr("Subject"),
		Weight:       weight,
		MasteryScore: mastery,
		Attempts:     attempts,
	}
}

// at is the TS default fixture topic({ masteryScore }).
func at(mastery float64) PlanTopic { return topic("Topic", &mastery, 5, 1) }

func base(topics []PlanTopic) PlanInputs {
	return PlanInputs{
		StartDate:          "2026-08-14",
		DaysRemaining:      30,
		DailyMinutes:       60,
		SecondsPerQuestion: ptr(90.0),
		Topics:             topics,
	}
}

func TestTopicNeed(t *testing.T) {
	cases := []struct {
		name          string
		higher, lower PlanTopic
	}{
		{"weak above strong", at(10), at(70)},
		{"heavier above lighter at same mastery", topic("Topic", ptr(50.0), 5, 2), topic("Topic", ptr(50.0), 5, 1)},
		// null mastery is the absence of a measurement, not a measurement of zero.
		{"never attempted above 40", topic("Topic", nil, 0, 1), at(40)},
		// The inversion the diagnostic exposed: measured weak must outrank never measured.
		{"measured weak above never measured", topic("Topic", ptr(29.0), 1, 1), topic("Topic", nil, 0, 1)},
		// Not knowing belongs between the two things you can know.
		{"never measured above measured solid", topic("Topic", nil, 0, 1), topic("Topic", ptr(78.0), 6, 1)},
	}
	for _, c := range cases {
		if h, l := TopicNeed(c.higher), TopicNeed(c.lower); !(h > l) {
			t.Errorf("%s: %v not > %v", c.name, h, l)
		}
	}

	// Nobody should walk into an exam having forgotten what they knew a month ago.
	if n := TopicNeed(at(100)); !(n > 0) {
		t.Errorf("mastered topic need = %v, want > 0", n)
	}
}

var promiseTopics = []PlanTopic{
	topic("Arrays", ptr(10.0), 3, 1.5),
	topic("Trees", ptr(35.0), 8, 1.2),
	topic("Sorting", ptr(70.0), 12, 1),
	topic("Graphs", nil, 0, 1.3),
}

func minutesPerDay(plan StudyPlan) map[string]int {
	m := map[string]int{}
	for _, s := range plan.Sessions {
		m[s.ScheduledDate] += s.PlannedMinutes
	}
	return m
}

func assertNoDayOver(t *testing.T, plan StudyPlan, limit int) {
	t.Helper()
	for date, minutes := range minutesPerDay(plan) {
		if minutes > limit {
			t.Errorf("%s is overbooked: %d > %d", date, minutes, limit)
		}
	}
}

func TestBuildPlanPromises(t *testing.T) {
	t.Run("never schedules a day beyond the learner's daily minutes", func(t *testing.T) {
		assertNoDayOver(t, BuildPlan(base(promiseTopics)), 60)
	})

	t.Run("never schedules a session too short to be worth doing", func(t *testing.T) {
		for _, s := range BuildPlan(base(promiseTopics)).Sessions {
			if s.PlannedMinutes < MinSessionMinutes {
				t.Errorf("session %+v shorter than %d", s, MinSessionMinutes)
			}
		}
	})

	t.Run("never schedules anything on or after the exam", func(t *testing.T) {
		in := base(promiseTopics)
		in.DaysRemaining = 10
		lastAllowed := dates.AddDays(in.StartDate, 9)
		for _, s := range BuildPlan(in).Sessions {
			if s.ScheduledDate > lastAllowed {
				t.Errorf("session on %s after %s", s.ScheduledDate, lastAllowed)
			}
		}
	})

	t.Run("never puts more topics in one day than a day can hold", func(t *testing.T) {
		in := base(promiseTopics)
		in.DailyMinutes = 480
		count := map[string]int{}
		for _, s := range BuildPlan(in).Sessions {
			count[s.ScheduledDate]++
		}
		for date, n := range count {
			if n > MaxTopicsPerDay {
				t.Errorf("%s holds %d topics", date, n)
			}
		}
	})

	t.Run("gives the weakest topic more total time than the strongest", func(t *testing.T) {
		byTopic := map[string]int{}
		for _, s := range BuildPlan(base(promiseTopics)).Sessions {
			byTopic[s.TopicName] += s.PlannedMinutes
		}
		if !(byTopic["Arrays"] > byTopic["Sorting"]) {
			t.Errorf("Arrays %d not > Sorting %d", byTopic["Arrays"], byTopic["Sorting"])
		}
	})

	t.Run("spreads topics across the first days", func(t *testing.T) {
		seen := map[string]bool{}
		for _, s := range BuildPlan(base(promiseTopics)).Sessions {
			if s.ScheduledDate <= dates.AddDays("2026-08-14", 2) {
				seen[s.TopicName] = true
			}
		}
		if len(seen) <= 1 {
			t.Errorf("first three days hold %d topics", len(seen))
		}
	})

	t.Run("explains every session in words", func(t *testing.T) {
		for _, s := range BuildPlan(base(promiseTopics)).Sessions {
			if len(s.Reason) <= 10 || !strings.Contains(s.Reason, s.TopicName) {
				t.Errorf("weak reason %q for %s", s.Reason, s.TopicName)
			}
		}
	})

	t.Run("estimates question counts from how fast the learner works", func(t *testing.T) {
		total := func(spq float64) int {
			in := base(promiseTopics)
			in.SecondsPerQuestion = &spq
			n := 0
			for _, s := range BuildPlan(in).Sessions {
				n += s.PlannedQuestions
			}
			return n
		}
		if fast, slow := total(30), total(180); !(fast > slow) {
			t.Errorf("fast %d not > slow %d", fast, slow)
		}
	})

	t.Run("always asks for at least one question per session", func(t *testing.T) {
		in := base(promiseTopics)
		in.SecondsPerQuestion = ptr(3600.0)
		for _, s := range BuildPlan(in).Sessions {
			if s.PlannedQuestions < 1 {
				t.Errorf("session %+v asks for no questions", s)
			}
		}
	})
}

func TestBuildPlanRevision(t *testing.T) {
	learn := []PlanTopic{topic("Arrays", ptr(10.0), 5, 1), topic("Trees", ptr(40.0), 5, 1)}
	due := []DueRevision{
		{TopicID: "r1", Name: "Normalisation", SubjectName: ptr("Databases"), DueOn: "2026-08-10", Lapses: 0, LastReviewAccuracy: ptr(85.0)},
		{TopicID: "r2", Name: "Time Complexity", SubjectName: ptr("Algorithms"), DueOn: "2026-08-12", Lapses: 2, LastReviewAccuracy: ptr(40.0)},
	}
	withDue := func(topics []PlanTopic, due []DueRevision) StudyPlan {
		in := base(topics)
		in.DueRevisions = due
		return BuildPlan(in)
	}

	t.Run("schedules due revisions on the earliest days", func(t *testing.T) {
		var revisions []PlannedSession
		for _, s := range withDue(learn, due).Sessions {
			if s.Kind == KindRevise {
				revisions = append(revisions, s)
			}
		}
		if len(revisions) != 2 {
			t.Fatalf("got %d revisions, want 2", len(revisions))
		}
		for _, s := range revisions {
			if s.ScheduledDate > dates.AddDays("2026-08-14", 1) {
				t.Errorf("revision on %s", s.ScheduledDate)
			}
		}
	})

	t.Run("does not let revision push a day past the daily minutes", func(t *testing.T) {
		assertNoDayOver(t, withDue(learn, due), 60)
	})

	t.Run("still leaves room for new learning on a revision day", func(t *testing.T) {
		learned := false
		for _, s := range withDue(learn, due).Sessions {
			learned = learned || (s.ScheduledDate == "2026-08-14" && s.Kind == KindLearn)
		}
		if !learned {
			t.Error("first day has no learning session")
		}
	})

	t.Run("spreads a large backlog over days", func(t *testing.T) {
		var backlog []DueRevision
		for i := range 12 {
			backlog = append(backlog, DueRevision{TopicID: fmt.Sprintf("r%d", i), Name: fmt.Sprintf("Old Topic %d", i), DueOn: "2026-08-01", LastReviewAccuracy: ptr(70.0)})
		}
		days := map[string]bool{}
		for _, s := range withDue(learn, backlog).Sessions {
			if s.Kind == KindRevise {
				days[s.ScheduledDate] = true
			}
		}
		if len(days) <= 1 {
			t.Errorf("backlog on %d day(s)", len(days))
		}
	})

	t.Run("explains a revision, and says when a topic keeps slipping", func(t *testing.T) {
		var reason string
		for _, s := range withDue(learn, due).Sessions {
			if s.TopicName == "Time Complexity" {
				reason = s.Reason
				break
			}
		}
		if !strings.Contains(reason, "slipped") || !strings.Contains(reason, "2") {
			t.Errorf("reason %q", reason)
		}
	})

	t.Run("never schedules the same topic twice on one day", func(t *testing.T) {
		shared := topic("Normalisation", ptr(30.0), 5, 1)
		plan := withDue(
			[]PlanTopic{shared, topic("Trees", ptr(40.0), 5, 1)},
			[]DueRevision{{TopicID: shared.TopicID, Name: shared.Name, SubjectName: ptr("Databases"), DueOn: "2026-08-10", Lapses: 1, LastReviewAccuracy: ptr(45.0)}},
		)
		seen := map[string]bool{}
		for _, s := range plan.Sessions {
			key := s.ScheduledDate + "|" + s.TopicID
			if seen[key] {
				t.Errorf("duplicate slot %s", key)
			}
			seen[key] = true
		}
	})

	t.Run("still gives a revised topic its learning time, on another day", func(t *testing.T) {
		shared := topic("Normalisation", ptr(20.0), 5, 1)
		plan := withDue(
			[]PlanTopic{shared, topic("Trees", ptr(40.0), 5, 1)},
			[]DueRevision{{TopicID: shared.TopicID, Name: shared.Name, SubjectName: ptr("Databases"), DueOn: "2026-08-10", Lapses: 0, LastReviewAccuracy: ptr(70.0)}},
		)
		learning := 0
		for _, s := range plan.Sessions {
			if s.TopicID == shared.TopicID && s.Kind == KindLearn {
				learning++
			}
		}
		if learning == 0 {
			t.Error("revised topic lost its learning time")
		}
	})

	t.Run("plans normally when nothing is due", func(t *testing.T) {
		plan := BuildPlan(base(learn))
		if len(plan.Sessions) == 0 {
			t.Fatal("no sessions")
		}
		for _, s := range plan.Sessions {
			if s.Kind != KindLearn {
				t.Errorf("unexpected %s session", s.Kind)
			}
		}
	})
}

func TestBuildPlanHonesty(t *testing.T) {
	t.Run("drops topics it cannot give a useful amount of time, and says which", func(t *testing.T) {
		var many []PlanTopic
		for i := range 20 {
			many = append(many, topic(fmt.Sprintf("Topic %d", i), ptr(20.0), 5, 1))
		}
		in := base(many)
		in.DaysRemaining = 1
		plan := BuildPlan(in)

		if len(plan.Sessions) > MaxTopicsPerDay {
			t.Errorf("%d sessions in one day", len(plan.Sessions))
		}
		if len(plan.OmittedTopics) == 0 {
			t.Error("nothing reported as omitted")
		}
		for _, o := range plan.OmittedTopics {
			if o.Name == "" || len(o.Reason) <= 10 {
				t.Errorf("omission not explained: %+v", o)
			}
		}
	})

	t.Run("plans for a learner whose whole day is shorter than the session floor", func(t *testing.T) {
		in := base([]PlanTopic{topic("Arrays", nil, 0, 1), topic("Trees", ptr(30.0), 5, 1)})
		in.DailyMinutes = 10
		in.DaysRemaining = 180
		plan := BuildPlan(in)

		if len(plan.Sessions) == 0 {
			t.Fatal("no sessions")
		}
		for _, s := range plan.Sessions {
			if s.PlannedMinutes > 10 || s.PlannedMinutes <= 0 || s.PlannedQuestions < 1 {
				t.Errorf("bad session %+v", s)
			}
		}
	})

	t.Run("rotates which topic comes first when a day only fits one", func(t *testing.T) {
		names := []string{"Arrays", "Trees", "Graphs", "Sorting", "Hashing"}
		var topics []PlanTopic
		for _, n := range names {
			topics = append(topics, topic(n, ptr(20.0), 5, 1))
		}
		in := base(topics)
		in.DailyMinutes = 10
		in.DaysRemaining = 60

		firstWeek := map[string]bool{}
		for _, s := range BuildPlan(in).Sessions {
			if s.ScheduledDate <= dates.AddDays("2026-08-14", 6) {
				firstWeek[s.TopicName] = true
			}
		}
		if len(firstWeek) < len(names)-1 {
			t.Errorf("met %d topics in week one, want >= %d", len(firstWeek), len(names)-1)
		}
	})

	t.Run("returns an empty plan for an exam that has already passed", func(t *testing.T) {
		in := base([]PlanTopic{at(50)})
		in.DaysRemaining = 0
		plan := BuildPlan(in)
		if plan.Sessions == nil || len(plan.Sessions) != 0 || plan.TotalMinutesPlanned != 0 {
			t.Errorf("got %+v", plan)
		}
	})

	t.Run("returns an empty plan when the goal has no topics in scope", func(t *testing.T) {
		plan := BuildPlan(base(nil))
		if plan.Sessions == nil || len(plan.Sessions) != 0 || plan.TopicsCovered != 0 {
			t.Errorf("got %+v", plan)
		}
	})

	t.Run("reports totals that match the sessions it produced", func(t *testing.T) {
		plan := BuildPlan(base([]PlanTopic{topic("Arrays", ptr(10.0), 5, 1), topic("Trees", ptr(60.0), 5, 1)}))
		summed := 0
		ids := map[string]bool{}
		for _, s := range plan.Sessions {
			summed += s.PlannedMinutes
			ids[s.TopicID] = true
		}
		if plan.TotalMinutesPlanned != summed {
			t.Errorf("total %d, summed %d", plan.TotalMinutesPlanned, summed)
		}
		if plan.TopicsCovered != len(ids) {
			t.Errorf("covered %d, distinct %d", plan.TopicsCovered, len(ids))
		}
		// The plan cannot promise more time than the learner said they had.
		if plan.TotalMinutesPlanned > 30*60 {
			t.Errorf("total %d exceeds budget", plan.TotalMinutesPlanned)
		}
	})
}
