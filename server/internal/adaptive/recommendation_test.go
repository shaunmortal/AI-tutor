package adaptive

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

func topic(mod func(*TopicCandidate)) TopicCandidate {
	t := TopicCandidate{
		TopicID: "t1", Name: "Topic", Weight: 1, MasteryScore: f(50), RecentAccuracy: f(0.5),
		RecentAttempts: 6, TotalAttempts: 10, DaysSinceLastAttempt: f(1),
	}
	if mod != nil {
		mod(&t)
	}
	return t
}

// untouched marks a candidate as never attempted.
func untouched(t *TopicCandidate) {
	t.MasteryScore, t.TotalAttempts, t.RecentAttempts, t.RecentAccuracy, t.DaysSinceLastAttempt = nil, 0, 0, nil, nil
}

func recommend(cs ...TopicCandidate) *NextBestAction {
	return RecommendNextAction(RecommendationInput{Candidates: cs})
}

func TestChooseDifficulty(t *testing.T) {
	cases := []struct {
		name     string
		mastery  float64
		accuracy float64
		attempts int
		want     Difficulty
		reason   string
	}{
		{"follows mastery: low", 20, 0.5, 6, Easy, ""},
		{"follows mastery: mid", 55, 0.5, 6, Medium, ""},
		{"follows mastery: high", 80, 0.5, 6, Hard, ""},
		{"steps back when recent answers go badly", 85, 0.2, 8, Medium, "(?i)stepping back"},
		{"steps up when recent answers are almost all right", 45, 0.95, 8, Hard, "(?i)stepping up"},
		// One unlucky answer must not drop a learner off hard questions.
		{"ignores recent form when there is barely any", 85, 0, 1, Hard, ""},
		{"cannot step below easy", 5, 0, 10, Easy, ""},
		{"cannot step above hard", 99, 1, 10, Hard, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ChooseDifficulty(DifficultySignals{MasteryScore: c.mastery, RecentAccuracy: f(c.accuracy), RecentAttempts: c.attempts})
			if got.Difficulty != c.want {
				t.Errorf("difficulty = %s, want %s", got.Difficulty, c.want)
			}
			if c.reason != "" && (got.Reason == nil || !regexp.MustCompile(c.reason).MatchString(*got.Reason)) {
				t.Errorf("reason = %v, want match %q", got.Reason, c.reason)
			}
		})
	}
}

func TestPriorityScore(t *testing.T) {
	cases := []struct {
		name          string
		higher, lower TopicCandidate
	}{
		{"weaker above stronger",
			topic(func(t *TopicCandidate) { t.MasteryScore = f(20) }),
			topic(func(t *TopicCandidate) { t.MasteryScore = f(80) })},
		{"unattempted is a full gap, not fine",
			topic(untouched),
			topic(func(t *TopicCandidate) { t.MasteryScore = f(30) })},
		{"stale above identical fresh",
			topic(func(t *TopicCandidate) { t.DaysSinceLastAttempt = f(30) }),
			topic(func(t *TopicCandidate) { t.DaysSinceLastAttempt = f(0) })},
		{"exam weight breaks a tie",
			topic(func(t *TopicCandidate) { t.Weight = 2 }),
			topic(func(t *TopicCandidate) { t.Weight = 1 })},
	}
	for _, c := range cases {
		if PriorityScore(c.higher) <= PriorityScore(c.lower) {
			t.Errorf("%s: %v <= %v", c.name, PriorityScore(c.higher), PriorityScore(c.lower))
		}
	}
}

func TestRecommendNothingInScope(t *testing.T) {
	if got := recommend(); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestRecommendWeakestImportant(t *testing.T) {
	r := recommend(
		topic(func(t *TopicCandidate) { t.TopicID, t.Name, t.MasteryScore = "strong", "Strong", f(88) }),
		topic(func(t *TopicCandidate) { t.TopicID, t.Name, t.MasteryScore, t.Weight = "weak", "Weak", f(25), 2 }),
	)
	if r.Topic.ID != "weak" || !strings.Contains(r.Reason, "Weak") || len(r.Reason) <= 20 {
		t.Fatalf("got %+v", r)
	}
}

func TestRecommendNeverAttemptedStartsGently(t *testing.T) {
	r := recommend(topic(func(t *TopicCandidate) { untouched(t); t.TopicID, t.Name = "new", "Brand New" }))
	if r.Action != LearnNew || r.Difficulty != Easy || r.QuestionCount > 3 {
		t.Fatalf("got %+v", r)
	}
}

func TestRecommendSolidNeglectedRevises(t *testing.T) {
	r := recommend(topic(func(t *TopicCandidate) {
		t.MasteryScore, t.DaysSinceLastAttempt, t.RecentAccuracy = f(82), f(21), f(0.8)
	}))
	if r.Action != Revise || !regexp.MustCompile(`(?i)days ago`).MatchString(r.Reason) {
		t.Fatalf("got %+v", r)
	}
}

func TestRecommendSolidRecentMiniTest(t *testing.T) {
	r := recommend(topic(func(t *TopicCandidate) {
		t.MasteryScore, t.DaysSinceLastAttempt, t.RecentAccuracy = f(85), f(1), f(0.8)
	}))
	if r.Action != MiniTest {
		t.Fatalf("action = %s", r.Action)
	}
}

func named(id string, mastery float64) TopicCandidate {
	return topic(func(t *TopicCandidate) { t.TopicID, t.Name, t.MasteryScore = id, strings.ToUpper(id), f(mastery) })
}

func altIDs(r *NextBestAction) []string {
	ids := []string{}
	for _, a := range r.Alternatives {
		ids = append(ids, a.TopicID)
	}
	return ids
}

func TestRecommendOffersRunnersUp(t *testing.T) {
	// All four are unfinished, so all four compete and the ranking is by need.
	r := recommend(named("a", 20), named("b", 30), named("c", 40), named("d", 45))
	if r.Topic.ID != "a" || !slices.Equal(altIDs(r), []string{"b", "c"}) {
		t.Fatalf("topic %s, alternatives %v", r.Topic.ID, altIDs(r))
	}
}

func TestRecommendNoSolidAlternativesWhileWeakRemain(t *testing.T) {
	r := recommend(named("weak", 20), named("solid", 80))
	if r.Topic.ID != "weak" || len(r.Alternatives) != 0 {
		t.Fatalf("topic %s, alternatives %v", r.Topic.ID, altIDs(r))
	}
}

func TestRecommendShrinksToAvailableTime(t *testing.T) {
	c := topic(func(t *TopicCandidate) { t.MasteryScore = f(30) })
	full := RecommendNextAction(RecommendationInput{Candidates: []TopicCandidate{c}, SecondsPerQuestion: f(60)})
	rushed := RecommendNextAction(RecommendationInput{Candidates: []TopicCandidate{c}, SecondsPerQuestion: f(60), AvailableMinutes: f(2)})
	if rushed.QuestionCount >= full.QuestionCount || rushed.QuestionCount != 2 || rushed.EstimatedMinutes > 2 {
		t.Fatalf("full %+v, rushed %+v", full, rushed)
	}
}

func TestRecommendNeverZeroQuestions(t *testing.T) {
	r := RecommendNextAction(RecommendationInput{Candidates: []TopicCandidate{topic(nil)}, SecondsPerQuestion: f(300), AvailableMinutes: f(1)})
	if r.QuestionCount < 1 || r.EstimatedMinutes < 2 {
		t.Fatalf("got %+v", r)
	}
}

func TestRecommendUsesLearnerPace(t *testing.T) {
	c := topic(func(t *TopicCandidate) { t.MasteryScore = f(30) })
	fast := RecommendNextAction(RecommendationInput{Candidates: []TopicCandidate{c}, SecondsPerQuestion: f(30)})
	slow := RecommendNextAction(RecommendationInput{Candidates: []TopicCandidate{c}, SecondsPerQuestion: f(180)})
	if slow.EstimatedMinutes <= fast.EstimatedMinutes {
		t.Fatalf("slow %d, fast %d", slow.EstimatedMinutes, fast.EstimatedMinutes)
	}
}

func TestRecommendFinishesStrugglingTopicFirst(t *testing.T) {
	// 1 out of 6 on Time Complexity is not a reason to go and start something else.
	r := recommend(
		topic(func(t *TopicCandidate) {
			t.TopicID, t.Name, t.MasteryScore, t.TotalAttempts, t.RecentAttempts, t.RecentAccuracy, t.DaysSinceLastAttempt =
				"struggling", "Time Complexity", f(22), 6, 6, f(0.17), f(0)
		}),
		topic(func(t *TopicCandidate) { untouched(t); t.TopicID, t.Name, t.Weight = "fresh", "SQL Queries", 2 }),
	)
	if r.Topic.ID != "struggling" || r.Action != Practice {
		t.Fatalf("got %s %s", r.Topic.ID, r.Action)
	}
}

func TestRecommendOpensNewTopicOnceStartedIsReasonable(t *testing.T) {
	r := recommend(
		topic(func(t *TopicCandidate) {
			t.TopicID, t.Name, t.MasteryScore, t.TotalAttempts, t.DaysSinceLastAttempt = "done-ish", "Sorting", f(64), 12, f(0)
		}),
		topic(func(t *TopicCandidate) { untouched(t); t.TopicID, t.Name = "fresh", "SQL Queries" }),
	)
	if r.Topic.ID != "fresh" || r.Action != LearnNew {
		t.Fatalf("got %s %s", r.Topic.ID, r.Action)
	}
}

func TestRecommendNothingStarted(t *testing.T) {
	r := recommend(
		topic(func(t *TopicCandidate) { untouched(t); t.TopicID, t.Name, t.Weight = "a", "A", 1 }),
		topic(func(t *TopicCandidate) { untouched(t); t.TopicID, t.Name, t.Weight = "b", "B", 2 }),
	)
	// Nothing to finish, so exam weight decides where to start.
	if r.Topic.ID != "b" {
		t.Fatalf("topic = %s", r.Topic.ID)
	}
}

func TestRecommendOrderIndependent(t *testing.T) {
	a := topic(func(t *TopicCandidate) { t.TopicID, t.Name, t.MasteryScore = "a", "Alpha", f(45) })
	b := topic(func(t *TopicCandidate) { t.TopicID, t.Name, t.MasteryScore = "b", "Beta", f(45) })
	if recommend(a, b).Topic.ID != recommend(b, a).Topic.ID {
		t.Fatal("order of input changed the answer")
	}
}
