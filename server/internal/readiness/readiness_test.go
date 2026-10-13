package readiness

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"testing"
)

// A readiness score is a claim about the future, so the properties worth pinning are the ones
// that would make it a dishonest claim: rewarding an untouched syllabus, punishing somebody for
// not having sat a mock, or moving the number when nothing was learned.

func f(v float64) *float64 { return &v }

func topic(mod func(*Topic)) Topic {
	subject := "Subject"
	t := Topic{TopicID: "t1", Name: "Topic", SubjectName: &subject, Weight: 1, MasteryScore: f(50), Attempts: 10}
	if mod != nil {
		mod(&t)
	}
	return t
}

func input(mod func(*Input)) Input {
	in := Input{Topics: []Topic{topic(nil)}, Mocks: []Mock{}, DaysRemaining: 60, DailyMinutes: 60}
	if mod != nil {
		mod(&in)
	}
	return in
}

// topics returns count topics, identical apart from the id and name.
func topics(count int, mastery *float64, attempts int) []Topic {
	out := make([]Topic, count)
	for i := range out {
		out[i] = topic(func(t *Topic) {
			t.TopicID, t.Name, t.MasteryScore, t.Attempts = fmt.Sprintf("t%d", i), fmt.Sprintf("Topic %d", i), mastery, attempts
		})
	}
	return out
}

func withTopics(ts []Topic) func(*Input) { return func(in *Input) { in.Topics = ts } }

func component(r Result, key string) *Component {
	for i := range r.Components {
		if r.Components[i].Key == key {
			return &r.Components[i]
		}
	}
	return nil
}

func names(gaps []Gap) []string {
	out := []string{}
	for _, g := range gaps {
		out = append(out, g.Name)
	}
	return out
}

// closeTo mirrors vitest's toBeCloseTo(want, digits).
func closeTo(got, want float64, digits int) bool {
	return math.Abs(got-want) < math.Pow(10, -float64(digits))/2
}

func TestUntouchedSyllabusNearTheFloor(t *testing.T) {
	// The planner gives an unmeasured topic a neutral prior. Readiness must not.
	r := Score(input(withTopics(topics(10, nil, 0))))
	if r.Score >= BuildingAt || r.Band != NotReady {
		t.Fatalf("score %v band %s", r.Score, r.Band)
	}
}

func TestMeasuredWeakAboveUnmeasured(t *testing.T) {
	measured := Score(input(withTopics(topics(10, f(30), 12))))
	unmeasured := Score(input(withTopics(topics(10, nil, 0))))
	if measured.Score <= unmeasured.Score {
		t.Fatalf("measured %v <= unmeasured %v", measured.Score, unmeasured.Score)
	}
}

func TestReadyAtTheMasteryTarget(t *testing.T) {
	r := Score(input(func(in *Input) {
		in.Topics = topics(10, f(MasteryTarget), 30)
		in.Mocks = []Mock{{88, "2026-08-10T10:00:00Z"}}
	}))
	if r.Band != Ready || r.Score < ReadyAt {
		t.Fatalf("score %v band %s", r.Score, r.Band)
	}
}

func TestNoMockIsNotPunished(t *testing.T) {
	// Scoring an absent mock as zero would make avoiding the measurement the winning move.
	noMock := Score(input(withTopics(topics(5, f(80), 40))))
	failedMock := Score(input(func(in *Input) {
		in.Topics = topics(5, f(80), 40)
		in.Mocks = []Mock{{20, "2026-08-10T10:00:00Z"}}
	}))
	if noMock.Score <= failedMock.Score || component(noMock, "mock") != nil {
		t.Fatalf("noMock %+v, failedMock %v", noMock, failedMock.Score)
	}
}

func TestWeightsAddUpToOne(t *testing.T) {
	// Dropping a component must not silently deflate the score, so survivors are renormalised.
	for i, in := range []Input{
		input(nil),
		input(func(in *Input) { in.Mocks = []Mock{{60, "2026-08-01T00:00:00Z"}} }),
		input(func(in *Input) { in.DaysRemaining = 0 }),
		input(func(in *Input) { in.DailyMinutes = 0 }),
	} {
		total := 0.0
		for _, c := range Score(in).Components {
			total += c.Weight
		}
		if !closeTo(total, 1, 2) {
			t.Errorf("case %d: weights total %v", i, total)
		}
	}
}

func TestTimeDroppedOnceExamArrives(t *testing.T) {
	if c := component(Score(input(func(in *Input) { in.DaysRemaining = 0 })), "time"); c != nil {
		t.Fatalf("time component present: %+v", c)
	}
}

func TestOnlyRecentMocksCount(t *testing.T) {
	var mocks []Mock
	for i := range 6 {
		mocks = append(mocks, Mock{10, fmt.Sprintf("2026-07-0%dT00:00:00Z", i+1)})
	}
	mocks = append(mocks, Mock{90, "2026-08-11T00:00:00Z"})

	mock := component(Score(input(func(in *Input) { in.Mocks = mocks })), "mock")
	// Three-deep window, one of which is the 90.
	if !closeTo(mock.Score, (90.0+10+10)/3, 1) {
		t.Fatalf("mock score %v", mock.Score)
	}
}

func TestMocksOrderedByEngine(t *testing.T) {
	ascending := []Mock{
		{10, "2026-06-01T00:00:00Z"},
		{20, "2026-07-01T00:00:00Z"},
		{90, "2026-08-01T00:00:00Z"},
		{95, "2026-08-10T00:00:00Z"},
	}
	descending := slices.Clone(ascending)
	slices.Reverse(descending)

	forwards := Score(input(func(in *Input) { in.Mocks = ascending }))
	backwards := Score(input(func(in *Input) { in.Mocks = descending }))
	if forwards.Score != backwards.Score {
		t.Fatalf("forwards %v, backwards %v", forwards.Score, backwards.Score)
	}
	if got := component(forwards, "mock").Score; !closeTo(got, (95.0+90+20)/3, 1) {
		t.Fatalf("mock score %v", got)
	}
}

func TestDeepAboveShallow(t *testing.T) {
	shallow := Score(input(withTopics(topics(10, f(20), 1))))
	deep := Score(input(withTopics(topics(10, f(80), 25))))
	if deep.Score <= shallow.Score {
		t.Fatalf("deep %v <= shallow %v", deep.Score, shallow.Score)
	}
}

func TestCoverageWeightedByImportance(t *testing.T) {
	tp := func(id string, weight float64, mastery *float64, attempts int) Topic {
		return topic(func(t *Topic) { t.TopicID, t.Weight, t.MasteryScore, t.Attempts = id, weight, mastery, attempts })
	}
	heavyDone := Score(input(withTopics([]Topic{tp("big", 5, f(70), 30), tp("s1", 0.5, nil, 0), tp("s2", 0.5, nil, 0)})))
	heavyMissing := Score(input(withTopics([]Topic{tp("big", 5, nil, 0), tp("s1", 0.5, f(70), 15), tp("s2", 0.5, f(70), 15)})))
	// Two topics done either way. Doing the one worth five times as much has to score higher.
	if heavyDone.Score <= heavyMissing.Score {
		t.Fatalf("heavyDone %v <= heavyMissing %v", heavyDone.Score, heavyMissing.Score)
	}
}

func named(id, name string, weight float64, mastery *float64, attempts int) Topic {
	return topic(func(t *Topic) {
		t.TopicID, t.Name, t.Weight, t.MasteryScore, t.Attempts = id, name, weight, mastery, attempts
	})
}

func TestGaps(t *testing.T) {
	cases := []struct {
		name           string
		topics         []Topic
		gaps           []string
		notStarted     []string // nil means not checked
		checkFirstOnly bool
	}{
		{"ranks the heaviest, weakest topic first",
			[]Topic{named("a", "Light and weak", 0.5, f(10), 10), named("b", "Heavy and weak", 3, f(10), 10), named("c", "Heavy and strong", 3, f(84), 10)},
			[]string{"Heavy and weak"}, nil, true},
		{"leaves out topics already at the target",
			[]Topic{named("done", "Topic", 1, f(MasteryTarget), 10), named("todo", "Not done", 1, f(40), 10)},
			[]string{"Not done"}, nil, false},
		// Different kinds of claim: ranked together the assumptions win, and among equally
		// weighted untouched topics the order came down to spelling.
		{"keeps measured weakness apart from what was never started",
			[]Topic{named("a", "Alpha, untouched", 1, nil, 0), named("b", "Bravo, untouched", 1, nil, 0), named("z", "Zulu, measured badly", 1, f(20), 14)},
			[]string{"Zulu, measured badly"}, []string{"Alpha, untouched", "Bravo, untouched"}, false},
		{"leaves a topic out of the measured list once it reaches the target",
			[]Topic{named("done", "Topic", 1, f(90), 30), named("todo", "Still short", 1, f(40), 12)},
			[]string{"Still short"}, nil, false},
		{"ranks what was never started by exam weight",
			[]Topic{named("l", "Light", 0.5, nil, 0), named("h", "Heavy", 4, nil, 0)},
			[]string{}, []string{"Heavy", "Light"}, false},
		// A list that reshuffles on refresh reads as the app changing its mind.
		{"breaks ties by name",
			[]Topic{named("b", "Beta", 1, f(40), 10), named("a", "Alpha", 1, f(40), 10)},
			[]string{"Alpha", "Beta"}, nil, false},
		{"breaks ties by name, reversed input",
			[]Topic{named("a", "Alpha", 1, f(40), 10), named("b", "Beta", 1, f(40), 10)},
			[]string{"Alpha", "Beta"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Score(input(withTopics(c.topics)))
			got := names(r.Gaps)
			if c.checkFirstOnly {
				got = got[:1]
			}
			if !slices.Equal(got, c.gaps) {
				t.Errorf("gaps = %v, want %v", got, c.gaps)
			}
			if c.notStarted != nil && !slices.Equal(names(r.NotStarted), c.notStarted) {
				t.Errorf("notStarted = %v, want %v", names(r.NotStarted), c.notStarted)
			}
		})
	}
}

func TestGapsCapped(t *testing.T) {
	if r := Score(input(withTopics(topics(40, f(10), 10)))); len(r.Gaps) > maxGaps {
		t.Fatalf("%d gaps", len(r.Gaps))
	}
}

func TestLowConfidenceFromAHandfulOfAnswers(t *testing.T) {
	r := Score(input(withTopics([]Topic{topic(func(t *Topic) { t.MasteryScore, t.Attempts = f(90), 3 })})))
	// And says so instead of leading with the number.
	if r.Confidence != Low || !regexp.MustCompile(`(?i)too early`).MatchString(r.Verdict) {
		t.Fatalf("confidence %s verdict %q", r.Confidence, r.Verdict)
	}
}

func TestHighConfidenceNeedsAMock(t *testing.T) {
	heavy := topics(10, f(80), 30)
	if c := Score(input(withTopics(heavy))).Confidence; c != Medium {
		t.Errorf("without mock: %s", c)
	}
	withMock := Score(input(func(in *Input) {
		in.Topics = heavy
		in.Mocks = []Mock{{80, "2026-08-10T00:00:00Z"}}
	}))
	if withMock.Confidence != High {
		t.Errorf("with mock: %s", withMock.Confidence)
	}
}

func TestEmptyScope(t *testing.T) {
	r := Score(input(func(in *Input) { in.Topics, in.Mocks = []Topic{}, []Mock{} }))
	if r.Score != 0 || !reflect.DeepEqual(r.Components, []Component{}) || !reflect.DeepEqual(r.Gaps, []Gap{}) ||
		!regexp.MustCompile(`(?i)set a goal`).MatchString(r.Verdict) {
		t.Fatalf("got %+v", r)
	}
}

func TestZeroWeightSyllabus(t *testing.T) {
	ts := topics(3, f(50), 10)
	for i := range ts {
		ts[i].Weight = 0
	}
	r := Score(input(withTopics(ts)))
	if math.IsNaN(r.Score) || math.IsInf(r.Score, 0) || r.Score < 0 {
		t.Fatalf("score %v", r.Score)
	}
}

func TestAbsurdInputsStayInRange(t *testing.T) {
	r := Score(input(func(in *Input) {
		in.Topics = topics(3, f(100), 9999)
		in.Mocks = []Mock{{100, "2026-08-10T00:00:00Z"}}
		in.DaysRemaining, in.DailyMinutes = 5000, 1440
	}))
	if r.Score > 100 {
		t.Errorf("score %v", r.Score)
	}
	for _, c := range r.Components {
		if c.Score < 0 || c.Score > 100 {
			t.Errorf("%s score %v", c.Key, c.Score)
		}
	}
}

func TestReportsCounts(t *testing.T) {
	r := Score(input(func(in *Input) {
		in.Topics = []Topic{
			named("a", "Topic", 1, f(90), 20),
			named("b", "Topic", 1, f(40), 5),
			named("c", "Topic", 1, nil, 0),
		}
		in.Mocks = []Mock{{70, "2026-08-10T00:00:00Z"}}
	}))
	if r.TopicsInScope != 3 || r.TopicsAttempted != 2 || r.TopicsMastered != 1 || r.MocksTaken != 1 {
		t.Fatalf("got %+v", r)
	}
}
