package validate

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The safety net on AI-generated content. Every case is something a model has plausibly
// produced: a shape that is fine but a question that is not.

// q is a good question; edit the copy to override.
func q(edit func(*GeneratedQuestion)) GeneratedQuestion {
	g := GeneratedQuestion{
		QuestionType:         "mcq",
		Body:                 "Which data structure gives constant-time lookup by key?",
		Options:              []string{"Linked list", "Hash table", "Sorted array", "Stack"},
		CorrectOptionIndexes: []int{1},
		Explanation:          "A hash table computes the position from the key itself, so the work does not grow with the number of entries.",
		Difficulty:           "easy",
	}
	if edit != nil {
		edit(&g)
	}
	return g
}

func TestBusinessCheckAcceptsGoodQuestions(t *testing.T) {
	for name, g := range map[string]GeneratedQuestion{
		"single-answer": q(nil),
		"multi-answer":  q(func(g *GeneratedQuestion) { g.QuestionType, g.CorrectOptionIndexes = "msq", []int{0, 2} }),
	} {
		if r := BusinessCheck(g); r != "" {
			t.Errorf("%s rejected: %s", name, r)
		}
	}
}

func TestBusinessCheckRejects(t *testing.T) {
	body := "Which data structure gives constant-time lookup by key?"
	cases := []struct {
		name string
		edit func(*GeneratedQuestion)
		want string
	}{
		// THE IMPORTANT ONE: this would mark a learner wrong for being right.
		{"answer index past the end", func(g *GeneratedQuestion) { g.CorrectOptionIndexes = []int{7} }, `(?i)does not exist`},
		{"single-answer with two answers", func(g *GeneratedQuestion) { g.CorrectOptionIndexes = []int{1, 2} }, `(?i)more than one correct`},
		{"multi-answer with one answer", func(g *GeneratedQuestion) { g.QuestionType, g.CorrectOptionIndexes = "msq", []int{1} }, `(?i)fewer than two`},
		{"same option correct twice", func(g *GeneratedQuestion) { g.QuestionType, g.CorrectOptionIndexes = "msq", []int{1, 1} }, `(?i)correct twice`},
		{"every option correct", func(g *GeneratedQuestion) {
			g.QuestionType, g.Options, g.CorrectOptionIndexes = "msq", []string{"a", "b"}, []int{0, 1}
		}, `(?i)every option`},
		{"two identical options", func(g *GeneratedQuestion) { g.Options = []string{"Hash table", "hash table ", "Stack", "Queue"} }, `(?i)identical`},
		{"all of the above", func(g *GeneratedQuestion) { g.Options = []string{"Hash table", "Stack", "Queue", "All of the above"} }, `(?i)own ordering`},
		{"refers to something unseen", func(g *GeneratedQuestion) { g.Body = "Based on the figure, which traversal is shown?" }, `(?i)cannot see`},
		{"explanation repeats the question", func(g *GeneratedQuestion) { g.Body, g.Explanation = body, body }, `(?i)repeats the question`},
		{"gives its own answer away", func(g *GeneratedQuestion) {
			g.Body = "Which structure is a hash table with constant-time lookup by key?"
			g.Options = []string{"a hash table with constant-time lookup by key", "Linked list", "Stack", "Queue"}
			g.CorrectOptionIndexes = []int{0}
		}, `(?i)contains the correct answer`},
	}
	for _, c := range cases {
		if r := BusinessCheck(q(c.edit)); !regexp.MustCompile(c.want).MatchString(r) {
			t.Errorf("%s: got %q, want match %s", c.name, r, c.want)
		}
	}
}

func batch(t *testing.T, qs ...GeneratedQuestion) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"questions": qs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateGeneratedQuestionsRejectsTheWrongShape(t *testing.T) {
	res := ValidateGeneratedQuestions([]byte(`{"questions": "four of them"}`))
	if len(res.Accepted) != 0 || len(res.Rejected) == 0 || !strings.Contains(res.Rejected[0].Reason, "shape") {
		t.Errorf("got %+v", res)
	}
	for _, raw := range []string{`"sorry, I cannot"`, `null`} {
		if n := len(ValidateGeneratedQuestions([]byte(raw)).Accepted); n != 0 {
			t.Errorf("%s: %d accepted", raw, n)
		}
	}
}

func TestValidateGeneratedQuestionsKeepsTheGoodOnes(t *testing.T) {
	res := ValidateGeneratedQuestions(batch(t,
		q(nil),
		q(func(g *GeneratedQuestion) { g.Body = "A second perfectly fine question about hashing?" }),
		q(func(g *GeneratedQuestion) { g.CorrectOptionIndexes = []int{9} }),
	))
	if len(res.Accepted) != 2 || len(res.Rejected) != 1 || res.Rejected[0].Index != 2 {
		t.Errorf("got %+v", res)
	}
}

func TestValidateGeneratedQuestionsDropsRepeats(t *testing.T) {
	res := ValidateGeneratedQuestions(batch(t, q(nil), q(nil)))
	if len(res.Accepted) != 1 || !regexp.MustCompile(`(?i)duplicate`).MatchString(res.Rejected[0].Reason) {
		t.Errorf("got %+v", res)
	}
}

// Schema rules the TS enforces through zod, which the TS tests do not exercise directly.
func TestSchemaRules(t *testing.T) {
	good := `"questionType":"mcq","body":"Which data structure gives constant-time lookup?","options":["a","b"],"explanation":"Because hashing maps a key straight to a slot.","difficulty":"easy"`
	cases := map[string]string{
		`{"questions":[]}`: "questions too small",
		`{"questions":[{` + good + `,"correctOptionIndexes":[1.0]}]}`:                                   "",
		`{"questions":[{` + good + `,"correctOptionIndexes":[0.5]}]}`:                                   "questions.0.correctOptionIndexes.0 invalid input: expected int",
		`{"questions":[{` + good + `,"correctOptionIndexes":[-1]}]}`:                                    "questions.0.correctOptionIndexes.0 too small",
		`{"questions":[{` + good + `,"correctOptionIndexes":[]}]}`:                                      "questions.0.correctOptionIndexes too small",
		`{"questions":[{` + good + `,"correctOptionIndexes":[0],"difficulty":"brutal"}]}`:               "questions.0.difficulty invalid option",
		`{"questions":[{` + good + `,"correctOptionIndexes":[0],"body":"Too short?"}]}`:                 "questions.0.body too small",
		`{"questions":[{` + good + `,"correctOptionIndexes":[0],"options":["a"]}]}`:                     "questions.0.options too small",
		`{"questions":[{` + good + `,"correctOptionIndexes":[0],"options":["a",""]}]}`:                  "questions.0.options.1 too small",
		strings.Replace(`{"questions":[{`+good+`,"correctOptionIndexes":[0]}]}`, `"body"`, `"Body"`, 1): "questions.0.body invalid input: expected string",
		`{"questions":[{"body":5}]}`: "questions.0.body invalid input",
	}
	for raw, want := range cases {
		res := ValidateGeneratedQuestions([]byte(raw))
		if want == "" {
			if len(res.Accepted) != 1 {
				t.Errorf("%s: want accepted, got %+v", raw, res)
			}
			continue
		}
		if len(res.Rejected) != 1 || res.Rejected[0].Index != -1 || !strings.Contains(res.Rejected[0].Reason, want) {
			t.Errorf("%s: want %q, got %+v", raw, want, res)
		}
	}
}

func TestParseAnswerCheck(t *testing.T) {
	c, err := ParseAnswerCheck([]byte(`{"correctOptionIndexes":[2,0],"confidence":"high","unanswerable":false,"extra":1}`))
	if err != nil || c.Confidence != "high" || len(c.CorrectOptionIndexes) != 2 {
		t.Errorf("good check: %+v, %v", c, err)
	}
	for _, raw := range []string{
		`{"correctOptionIndexes":[0,1,2,3,4,5,6],"confidence":"high","unanswerable":false}`,
		`{"correctOptionIndexes":[0],"confidence":"sure","unanswerable":false}`,
		`{"correctOptionIndexes":[0],"confidence":"high"}`,
		`{"correctOptionIndexes":[0],"confidence":"high","unanswerable":false,"note":null}`,
		`null`,
	} {
		if _, err := ParseAnswerCheck([]byte(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}

func TestAgreesWithProposedAnswer(t *testing.T) {
	confident := func(idx ...int) AnswerCheck {
		return AnswerCheck{CorrectOptionIndexes: idx, Confidence: "high"}
	}
	cases := []struct {
		name     string
		proposed []int
		check    AnswerCheck
		verified bool
		reason   string
	}{
		{"independent check agrees", []int{1}, confident(1), true, ""},
		{"ordering ignored", []int{2, 0}, confident(0, 2), true, ""},
		// THE IMPORTANT ONE.
		{"check answered differently", []int{1}, confident(2), false, `answered \[2\]`},
		{"unanswerable", []int{1}, AnswerCheck{CorrectOptionIndexes: []int{1}, Confidence: "high", Unanswerable: true}, false, `(?i)unanswerable`},
		{"not confident, even if it agreed", []int{1}, AnswerCheck{CorrectOptionIndexes: []int{1}, Confidence: "low"}, false, `(?i)not confident`},
	}
	for _, c := range cases {
		ok, reason := AgreesWithProposedAnswer(c.proposed, c.check)
		if ok != c.verified || !regexp.MustCompile(c.reason).MatchString(reason) {
			t.Errorf("%s: got (%v, %q)", c.name, ok, reason)
		}
	}
}

func TestTypedQuestions(t *testing.T) {
	short := q(func(g *GeneratedQuestion) {
		g.QuestionType, g.Options, g.CorrectOptionIndexes = "short", nil, nil
		g.Body = "What is the name of the structure that maps keys to slots by hashing?"
		g.AcceptedAnswers, g.Rubric = []string{"hash table", "hash map"}, "Names a hash table (or hash map)."
	})
	numeric := q(func(g *GeneratedQuestion) {
		g.QuestionType, g.Options, g.CorrectOptionIndexes = "numeric", nil, nil
		g.Body = "A table of 8 slots holds 6 keys. What is its load factor?"
		g.NumericAnswer = &NumericAnswer{Value: 0.75, Tolerance: 0.01}
	})
	for name, g := range map[string]GeneratedQuestion{"short": short, "numeric": numeric} {
		if r := BusinessCheck(g); r != "" {
			t.Errorf("%s rejected: %s", name, r)
		}
	}
	for name, edit := range map[string]func(*GeneratedQuestion){
		"short with no accepted answer": func(g *GeneratedQuestion) { g.AcceptedAnswers = nil },
		"short with no rubric":          func(g *GeneratedQuestion) { g.Rubric = " " },
		"short with options":            func(g *GeneratedQuestion) { g.Options = []string{"a", "b"} },
		"short giving itself away": func(g *GeneratedQuestion) {
			g.AcceptedAnswers = []string{"separate chaining"}
			g.Body = "Is separate chaining a way to resolve hash collisions? Name the method."
		},
	} {
		g := short
		edit(&g)
		if BusinessCheck(g) == "" {
			t.Errorf("%s accepted", name)
		}
	}
	noKey := numeric
	noKey.NumericAnswer = nil
	if BusinessCheck(noKey) == "" {
		t.Error("numeric with no answer accepted")
	}

	// The schema gate accepts typed questions without options, and lines misconceptions up.
	raw := `{"questions": [
		{"questionType": "short", "body": "` + short.Body + `", "options": [], "correctOptionIndexes": [],
		 "optionMisconceptions": [], "acceptedAnswers": ["hash table"], "rubric": "Names a hash table.",
		 "numericAnswer": null, "explanation": "` + short.Explanation + `", "difficulty": "easy"},
		{"questionType": "mcq", "body": "` + q(nil).Body + `", "options": ["Linked list", "Hash table", "Sorted array", "Stack"],
		 "correctOptionIndexes": [1], "optionMisconceptions": ["thinks lists index by key", "wrongly labelled", "", "confuses LIFO with lookup"],
		 "acceptedAnswers": [], "rubric": "", "numericAnswer": null, "explanation": "` + q(nil).Explanation + `", "difficulty": "easy"}
	]}`
	out := ValidateGeneratedQuestions([]byte(raw))
	if len(out.Accepted) != 2 {
		t.Fatalf("accepted %d: %+v", len(out.Accepted), out.Rejected)
	}
	got := out.Accepted[1].OptionMisconceptions
	if len(got) != 4 || got[0] != "thinks lists index by key" || got[1] != "" || got[3] != "confuses LIFO with lookup" {
		t.Errorf("misconceptions not aligned with the correct option blanked: %q", got)
	}
}

func TestAgreesWithNumericAnswer(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	key := NumericAnswer{Value: 0.75, Tolerance: 0.01}
	for _, c := range []struct {
		check TypedCheck
		want  bool
	}{
		{TypedCheck{NumericValue: v(0.755), Confidence: "high"}, true},
		{TypedCheck{NumericValue: v(0.8), Confidence: "high"}, false},
		{TypedCheck{NumericValue: v(0.75), Confidence: "low"}, false},
		{TypedCheck{NumericValue: nil, Confidence: "high"}, false},
		{TypedCheck{NumericValue: v(0.75), Confidence: "high", Unanswerable: true}, false},
	} {
		if got, reason := AgreesWithNumericAnswer(key, c.check); got != c.want {
			t.Errorf("%+v: got %v (%s)", c.check, got, reason)
		}
	}
	if _, err := ParseTypedCheck([]byte(`{"answer": "x", "numericValue": null, "confidence": "high", "unanswerable": false}`)); err != nil {
		t.Error(err)
	}
	if _, err := ParseTypedCheck([]byte(`{"answer": "x", "confidence": "sure", "unanswerable": false}`)); err == nil {
		t.Error("bad confidence accepted")
	}
}
