package assessment

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
)

// The allocation is where a mock test quietly goes wrong — serving nine of fifteen, spreading
// one per topic, asking a topic the bank cannot fill — so these are mostly arithmetic invariants.

func ptr[T any](v T) *T { return &v }

// mt is a topic with the TS test's defaults; mutate the copy to override.
func mt(name string, edit func(*MockTestTopic)) MockTestTopic {
	t := MockTestTopic{
		TopicID: "t-" + name, Name: name, SubjectName: ptr("Subject"), Weight: 1,
		MasteryScore: ptr(50.0), PlannedSessions: 1, AvailableQuestions: 20,
	}
	if edit != nil {
		edit(&t)
	}
	return t
}

func many(n int) []MockTestTopic {
	out := make([]MockTestTopic, n)
	for i := range out {
		out[i] = mt(fmt.Sprintf("Topic %d", i), func(t *MockTestTopic) { t.TopicID = fmt.Sprintf("t-%d", i) })
	}
	return out
}

func TestTopicShare(t *testing.T) {
	cases := []struct {
		name       string
		more, less func(*MockTestTopic)
	}{
		{"weak beats strong", func(t *MockTestTopic) { t.MasteryScore = ptr(15.0) }, func(t *MockTestTopic) { t.MasteryScore = ptr(80.0) }},
		// This is what makes it a test of the plan rather than of the syllabus.
		{"heavily scheduled beats barely scheduled", func(t *MockTestTopic) { t.PlannedSessions = 4 }, func(t *MockTestTopic) { t.PlannedSessions = 0 }},
		{"heavier beats lighter at equal mastery", func(t *MockTestTopic) { t.Weight = 2 }, func(t *MockTestTopic) { t.Weight = 1 }},
		// nil mastery is the absence of a measurement; reading it as zero would be a measurement.
		{"never attempted needs the most", func(t *MockTestTopic) { t.MasteryScore = nil }, func(t *MockTestTopic) { t.MasteryScore = ptr(60.0) }},
	}
	for _, c := range cases {
		if a, b := TopicShare(mt("x", c.more)), TopicShare(mt("x", c.less)); a <= b {
			t.Errorf("%s: %v <= %v", c.name, a, b)
		}
	}
}

func fourTopics() []MockTestTopic {
	return []MockTestTopic{
		mt("Arrays", func(t *MockTestTopic) { t.TopicID, t.MasteryScore, t.Weight = "t-a", ptr(20.0), 1.5 }),
		mt("Trees", func(t *MockTestTopic) { t.TopicID, t.MasteryScore = "t-b", ptr(45.0) }),
		mt("Sorting", func(t *MockTestTopic) { t.TopicID, t.MasteryScore = "t-c", ptr(75.0) }),
		mt("Graphs", func(t *MockTestTopic) { t.TopicID, t.MasteryScore = "t-d", nil }),
	}
}

func TestPlanMockTestServesWhatWasAsked(t *testing.T) {
	plan := PlanMockTest(fourTopics(), 15, 90)
	sum := 0
	for _, a := range plan.Allocations {
		sum += a.QuestionCount
		// Never so thin that a topic score means nothing.
		if a.QuestionCount < MockMinPerTopic {
			t.Errorf("%s has %d questions", a.Name, a.QuestionCount)
		}
	}
	if plan.TotalQuestions != 15 || sum != 15 {
		t.Fatalf("total %d, sum %d, want 15", plan.TotalQuestions, sum)
	}
}

func TestPlanMockTestCapsTopics(t *testing.T) {
	if n := len(PlanMockTest(many(15), 20, 90).Allocations); n > MockMaxTopics {
		t.Fatalf("%d topics", n)
	}
}

func TestPlanMockTestWeakestGetsMore(t *testing.T) {
	counts := map[string]int{}
	for _, a := range PlanMockTest(fourTopics(), 20, 90).Allocations {
		counts[a.Name] = a.QuestionCount
	}
	if counts["Arrays"] <= counts["Sorting"] {
		t.Fatalf("counts: %v", counts)
	}
}

func TestPlanMockTestRespectsTheBank(t *testing.T) {
	thin := []MockTestTopic{
		mt("Thin", func(t *MockTestTopic) { t.TopicID, t.AvailableQuestions, t.MasteryScore = "t-thin", 3, ptr(10.0) }),
		mt("Deep", func(t *MockTestTopic) { t.TopicID, t.AvailableQuestions = "t-deep", 40 }),
	}
	avail := map[string]int{"t-thin": 3, "t-deep": 40}
	for _, a := range PlanMockTest(thin, 20, 90).Allocations {
		if a.QuestionCount > avail[a.TopicID] {
			t.Errorf("%s: %d > %d", a.TopicID, a.QuestionCount, avail[a.TopicID])
		}
	}

	// A shorter test rather than a wrong one when the bank cannot fill it.
	plan := PlanMockTest([]MockTestTopic{mt("Only", func(t *MockTestTopic) { t.TopicID, t.AvailableQuestions = "t-o", 4 })}, 20, 90)
	if plan.TotalQuestions != 4 || len(plan.Allocations) != 1 {
		t.Fatalf("got %+v", plan)
	}
}

func TestPlanMockTestClampsLength(t *testing.T) {
	if n := PlanMockTest(fourTopics(), 500, 90).TotalQuestions; n > MockMaxQuestions {
		t.Errorf("500 requested: %d", n)
	}
	if n := PlanMockTest(fourTopics(), 1, 90).TotalQuestions; n < MockMinQuestions {
		t.Errorf("1 requested: %d", n)
	}
}

func TestPlanMockTestNamesOmitted(t *testing.T) {
	topics := append(many(10), mt("Empty", func(t *MockTestTopic) { t.TopicID, t.AvailableQuestions = "t-empty", 0 }))
	plan := PlanMockTest(topics, 15, 90)
	if len(plan.Omitted) == 0 {
		t.Fatal("nothing omitted")
	}
	sawEmpty := false
	for _, o := range plan.Omitted {
		if o.Name == "" || len(o.Reason) <= 8 {
			t.Errorf("bad omission %+v", o)
		}
		sawEmpty = sawEmpty || o.Name == "Empty"
	}
	if !sawEmpty {
		t.Error("topic with no questions not named")
	}
}

func TestPlanMockTestEmpty(t *testing.T) {
	for _, topics := range [][]MockTestTopic{nil, {mt("x", func(t *MockTestTopic) { t.AvailableQuestions = 0 })}} {
		if a := PlanMockTest(topics, 15, 90).Allocations; a == nil || len(a) != 0 {
			t.Errorf("allocations %v, want []", a)
		}
	}
}

func TestPlanMockTestDuration(t *testing.T) {
	fast, slow := PlanMockTest(fourTopics(), 15, 45), PlanMockTest(fourTopics(), 15, 200)
	if slow.DurationMinutes <= fast.DurationMinutes || fast.DurationMinutes <= 0 {
		t.Errorf("fast %d, slow %d", fast.DurationMinutes, slow.DurationMinutes)
	}
	// A measured 10 s/question came from practice, where the answer appears after each one.
	if d := PlanMockTest(fourTopics(), 15, 10).DurationMinutes; d < int(jsRound(15*45/60.0)) {
		t.Errorf("floored pace: %d minutes", d)
	}
}

func TestPlanMockTestExplainsEveryTopic(t *testing.T) {
	for _, a := range PlanMockTest(fourTopics(), 15, 90).Allocations {
		if !strings.Contains(a.Reason, a.Name) || len(a.Reason) <= 15 {
			t.Errorf("reason %q for %s", a.Reason, a.Name)
		}
	}
}

func mq(topicID string, isCorrect bool, answered ...bool) MarkedQuestion {
	return MarkedQuestion{TopicID: topicID, IsCorrect: isCorrect, Answered: len(answered) == 0 || answered[0]}
}

var topicNames = map[string]string{"t-a": "Arrays", "t-b": "Trees"}

func near(a, b float64) bool { return math.Abs(a-b) < 0.05 } // toBeCloseTo(b, 1)

func TestMarkMockTest(t *testing.T) {
	res := MarkMockTest([]MarkedQuestion{mq("t-a", true), mq("t-a", true), mq("t-b", false), mq("t-b", false)}, topicNames)
	if res.CorrectCount != 2 || res.TotalQuestions != 4 || res.ScorePercent != 50 {
		t.Errorf("scoring: %+v", res)
	}
	// Names the weakest topic, so there is something to do next.
	if !strings.Contains(res.Verdict, "Trees") {
		t.Errorf("verdict %q", res.Verdict)
	}

	// Skipped scores zero but is reported separately.
	res = MarkMockTest([]MarkedQuestion{mq("t-a", true), mq("t-a", false, false), mq("t-a", false, false)}, topicNames)
	if !near(res.ScorePercent, 33.3) || res.AnsweredCount != 1 || res.TotalQuestions != 3 ||
		!regexp.MustCompile(`(?i)blank`).MatchString(res.Verdict) {
		t.Errorf("skipped: %+v", res)
	}

	res = MarkMockTest([]MarkedQuestion{mq("t-a", true), mq("t-a", true), mq("t-b", false), mq("t-b", false), mq("t-b", true)}, topicNames)
	if res.ByTopic[0].TopicID != "t-b" || !near(res.ByTopic[0].Percent, 33.3) || res.ByTopic[1].Percent != 100 {
		t.Errorf("by topic: %+v", res.ByTopic)
	}

	res = MarkMockTest([]MarkedQuestion{mq("t-a", true), mq("t-a", true)}, topicNames)
	if !res.Passed || res.ScorePercent != 100 || !regexp.MustCompile(`(?i)strong`).MatchString(res.Verdict) {
		t.Errorf("went well: %+v", res)
	}

	res = MarkMockTest([]MarkedQuestion{mq("t-a", false, false), mq("t-a", false, false)}, topicNames)
	if res.ScorePercent != 0 || !regexp.MustCompile(`(?i)nothing`).MatchString(res.Verdict) {
		t.Errorf("not attempted: %+v", res)
	}

	res = MarkMockTest(nil, topicNames)
	if res.ScorePercent != 0 || res.ByTopic == nil || len(res.ByTopic) != 0 {
		t.Errorf("empty: %+v", res)
	}
}

func TestJSRound(t *testing.T) {
	for x, want := range map[float64]float64{2.5: 3, -2.5: -2, 0.49999999999999994: 0, 1.4: 1, -1.6: -2} {
		if got := jsRound(x); got != want {
			t.Errorf("jsRound(%v) = %v, want %v", x, got, want)
		}
	}
}
