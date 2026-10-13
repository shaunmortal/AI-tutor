package assessment

import (
	"regexp"
	"testing"
)

func opts(o ...int) GivenAnswer {
	if o == nil {
		o = []int{}
	}
	return GivenAnswer{SelectedOptions: o}
}

func val(v float64) GivenAnswer { return GivenAnswer{Value: &v} }

func TestGradeAnswer(t *testing.T) {
	mcq := GradableQuestion{MCQ, []byte(`[2]`)}
	msq := GradableQuestion{MSQ, []byte(`[0, 2, 3]`)}
	num := GradableQuestion{Numeric, []byte(`{"value": 0.5, "tol": 0.01}`)}
	thirds := GradableQuestion{Numeric, []byte(`{"value": 0.3333333333333333, "tol": 0.01}`)}

	cases := []struct {
		name  string
		q     GradableQuestion
		given GivenAnswer
		want  bool
	}{
		{"mcq accepts the right option", mcq, opts(2), true},
		{"mcq rejects a wrong option", mcq, opts(1), false},
		{"mcq rejects the right option plus another", mcq, opts(2, 3), false},
		{"mcq rejects answering nothing", mcq, opts(), false},
		{"msq accepts every correct option regardless of order", msq, opts(3, 0, 2), true},
		// Two of three correct is not knowing the concept.
		{"msq gives no credit for a partly right answer", msq, opts(0, 2), false},
		{"msq rejects an extra wrong option", msq, opts(0, 1, 2, 3), false},
		{"msq cannot be fooled by repeating one option", msq, opts(0, 0, 0), false},
		{"numeric accepts an exact answer", num, val(0.5), true},
		{"numeric accepts an answer inside the tolerance", num, val(0.505), true},
		{"numeric rejects an answer outside the tolerance", num, val(0.6), false},
		{"numeric accepts 1/3 typed as 0.33", thirds, val(0.33), true},
	}
	for _, c := range cases {
		got, err := GradeAnswer(c.q, c.given)
		if err != nil || got != c.want {
			t.Errorf("%s: got (%v, %v), want %v", c.name, got, err, c.want)
		}
	}
}

func TestGradeAnswerRefusesToGuess(t *testing.T) {
	cases := []struct {
		name  string
		q     GradableQuestion
		given GivenAnswer
		want  string
	}{
		{"options sent for a numeric question", GradableQuestion{Numeric, []byte(`{"value": 1, "tol": 0}`)}, opts(0), `(?i)numeric value`},
		{"a number sent for a choice question", GradableQuestion{MCQ, []byte(`[0]`)}, val(1), `(?i)selected options`},
		// An AI-generated question could store anything; fail loudly rather than mark wrong.
		{"stored answer is the wrong shape", GradableQuestion{MCQ, []byte(`"option B"`)}, opts(1), `(?i)cannot be graded`},
		{"single-choice question storing two answers", GradableQuestion{MCQ, []byte(`[0, 1]`)}, opts(0), `(?i)cannot be graded`},
	}
	for _, c := range cases {
		_, err := GradeAnswer(c.q, c.given)
		if err == nil || !regexp.MustCompile(c.want).MatchString(err.Error()) {
			t.Errorf("%s: got error %v, want match %s", c.name, err, c.want)
		}
	}
}
