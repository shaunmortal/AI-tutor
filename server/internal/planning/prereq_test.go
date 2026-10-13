package planning

import "testing"

// firstLearn is the index of a topic's first learning session, or -1.
func firstLearn(p StudyPlan, topicID string) int {
	for i, s := range p.Sessions {
		if s.TopicID == topicID && s.Kind == KindLearn {
			return i
		}
	}
	return -1
}

func TestPrerequisitesComeFirst(t *testing.T) {
	// The dependent topic is the weakest and heaviest, so on need alone it would open the plan.
	dependent := topic("Joins", ptr(10.0), 5, 3)
	basis := topic("Keys", ptr(30.0), 5, 1)
	in := base([]PlanTopic{dependent, basis})
	in.DailyMinutes = 30 // one topic a day, so order is a matter of days

	without := BuildPlan(in)
	if firstLearn(without, dependent.TopicID) > firstLearn(without, basis.TopicID) {
		t.Fatalf("fixture: expected the dependent topic first without prerequisites")
	}

	in.Prerequisites = map[string][]string{dependent.TopicID: {basis.TopicID}}
	with := BuildPlan(in)
	d, b := firstLearn(with, dependent.TopicID), firstLearn(with, basis.TopicID)
	// Sessions are in day order and, within a day, in sort order: earlier the same day is fine.
	if b < 0 || d < 0 || b > d {
		t.Fatalf("prerequisite not first: basis at %d, dependent at %d: %+v", b, d, with.Sessions[:4])
	}
	// Order only: the minutes are the same.
	if with.TotalMinutesPlanned != without.TotalMinutesPlanned {
		t.Fatalf("minutes changed: %d vs %d", with.TotalMinutesPlanned, without.TotalMinutesPlanned)
	}
}

func TestStrongPrerequisiteDoesNotHoldAnythingUp(t *testing.T) {
	dependent := topic("Joins", ptr(10.0), 5, 3)
	basis := topic("Keys", ptr(80.0), 5, 1)
	in := base([]PlanTopic{dependent, basis})
	in.DailyMinutes = 30
	in.Prerequisites = map[string][]string{dependent.TopicID: {basis.TopicID}}
	if p := BuildPlan(in); firstLearn(p, dependent.TopicID) != 0 {
		t.Fatalf("a solid prerequisite delayed its dependent: %+v", p.Sessions[:2])
	}
}

func TestPrerequisiteCycleDoesNotStallThePlan(t *testing.T) {
	a, b := topic("A", nil, 0, 1), topic("B", nil, 0, 1)
	in := base([]PlanTopic{a, b})
	in.Prerequisites = map[string][]string{a.TopicID: {b.TopicID}, b.TopicID: {a.TopicID}}
	p := BuildPlan(in)
	if len(p.Sessions) == 0 || p.Sessions[0].ScheduledDate != in.StartDate || len(p.OmittedTopics) != 0 {
		t.Fatalf("cycle stalled the plan: %d sessions, omitted %v", len(p.Sessions), p.OmittedTopics)
	}
}

func TestSelfRatedPriorShiftsNeed(t *testing.T) {
	confident, unsure, unknown := topic("A", nil, 0, 1), topic("B", nil, 0, 1), topic("C", nil, 0, 1)
	confident.PriorMastery, unsure.PriorMastery = ptr(65.0), ptr(15.0)
	if !(TopicNeed(unsure) > TopicNeed(unknown) && TopicNeed(unknown) > TopicNeed(confident)) {
		t.Fatalf("needs: unsure %v unknown %v confident %v", TopicNeed(unsure), TopicNeed(unknown), TopicNeed(confident))
	}
	// Measured mastery always wins over the prior.
	measured := topic("D", ptr(80.0), 3, 1)
	measured.PriorMastery = ptr(15.0)
	if TopicNeed(measured) != TopicNeed(topic("E", ptr(80.0), 3, 1)) {
		t.Fatal("prior overrode a measured score")
	}
}
