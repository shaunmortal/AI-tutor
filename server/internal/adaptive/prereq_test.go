package adaptive

import (
	"strings"
	"testing"
)

func TestWeakPrerequisiteIsRecommendedFirst(t *testing.T) {
	joins := topic(func(c *TopicCandidate) {
		c.TopicID, c.Name, c.MasteryScore, c.Weight, c.Prerequisites = "joins", "Joins", f(10), 3, []string{"keys"}
	})
	keys := topic(func(c *TopicCandidate) { c.TopicID, c.Name, c.MasteryScore = "keys", "Keys", f(40) })
	other := topic(func(c *TopicCandidate) { c.TopicID, c.Name, c.MasteryScore = "other", "Other", f(45) })

	if got := recommend(joins, other); got.Topic.ID != "joins" {
		t.Fatalf("fixture: joins should win on need, got %s", got.Topic.ID)
	}
	got := recommend(joins, keys, other)
	if got.Topic.ID != "keys" || !strings.Contains(got.Reason, "It comes before Joins, which builds on it.") {
		t.Fatalf("got %s: %s", got.Topic.ID, got.Reason)
	}
	// The topic it displaced is still offered.
	if len(got.Alternatives) == 0 || got.Alternatives[0].TopicID != "joins" {
		t.Fatalf("alternatives: %+v", got.Alternatives)
	}
}

func TestPrerequisiteChainAndLimits(t *testing.T) {
	c := topic(func(x *TopicCandidate) {
		x.TopicID, x.Name, x.MasteryScore, x.Prerequisites = "c", "C", f(5), []string{"b"}
	})
	b := topic(func(x *TopicCandidate) {
		x.TopicID, x.Name, x.MasteryScore, x.Prerequisites = "b", "B", f(40), []string{"a"}
	})
	a := topic(func(x *TopicCandidate) {
		x.TopicID, x.Name, x.MasteryScore, x.Prerequisites = "a", "A", f(30), []string{"c"}
	}) // cycle back to c
	if got, unlocks := WeakPrerequisiteFirst(c, []TopicCandidate{a, b, c}); got.TopicID != "a" || unlocks == nil || unlocks.ID != "c" {
		t.Fatalf("chain: got %s, unlocks %+v", got.TopicID, unlocks)
	}

	// A solid prerequisite, or one outside the scope, changes nothing.
	solid := topic(func(x *TopicCandidate) { x.TopicID, x.MasteryScore = "b", f(80) })
	if got, unlocks := WeakPrerequisiteFirst(c, []TopicCandidate{c, solid}); got.TopicID != "c" || unlocks != nil {
		t.Fatalf("solid prerequisite: got %s", got.TopicID)
	}
	if got, _ := WeakPrerequisiteFirst(c, []TopicCandidate{c}); got.TopicID != "c" {
		t.Fatalf("out of scope: got %s", got.TopicID)
	}
}

func TestSelfRatedPriorInPriority(t *testing.T) {
	confident := topic(untouched)
	confident.PriorMastery = f(65)
	if PriorityScore(confident) >= PriorityScore(topic(untouched)) {
		t.Fatal("a confident self-rating should lower an unmeasured topic's priority")
	}
}
