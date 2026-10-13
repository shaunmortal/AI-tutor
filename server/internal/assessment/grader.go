// Package assessment holds the pure engines behind practice grading, the diagnostic and
// mock tests: what goes into a paper, whether an answer is right, and what a score means.
// No database, no AI — every rule here is cheap to unit test.
package assessment

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// QuestionType is how a question is answered.
type QuestionType string

// Question types.
const (
	MCQ     QuestionType = "mcq"
	MSQ     QuestionType = "msq"
	Numeric QuestionType = "numeric"
	// Short is a typed word or phrase, marked against accepted answers and a rubric.
	Short QuestionType = "short"
)

// ErrNeedsJudgement means a typed answer matched none of the accepted answers, so only a rubric
// grade (a model call, made by the caller) can decide it.
var ErrNeedsJudgement = errors.New("this answer needs a rubric grade")

// ShortKey is a short question's stored answer: {"answers": [...], "rubric": "..."}.
type ShortKey struct {
	Answers []string `json:"answers"`
	Rubric  string   `json:"rubric"`
}

// ParseShortKey reads a short question's answer key, checking its shape rather than trusting it.
func ParseShortKey(raw jsontext.Value) (ShortKey, error) {
	var k ShortKey
	if json.Unmarshal(raw, &k) != nil || len(k.Answers) == 0 || strings.TrimSpace(k.Rubric) == "" {
		return k, malformed("its stored answer is not a list of accepted answers with a rubric")
	}
	return k, nil
}

// NormaliseShort is what two typed answers must agree on to be the same answer: case, spacing,
// punctuation and a leading article do not matter ("The Mitochondria." is "mitochondria").
func NormaliseShort(s string) string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' })
	for i, f := range fields {
		fields[i] = strings.Trim(f, ".") // a full stop ends a sentence; inside a number it stays
	}
	fields = slices.DeleteFunc(fields, func(f string) bool { return f == "" })
	if len(fields) > 1 && (fields[0] == "the" || fields[0] == "a" || fields[0] == "an") {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

// MatchesShort reports whether answer is one of the accepted answers, after normalising both.
func MatchesShort(k ShortKey, answer string) bool {
	given := NormaliseShort(answer)
	return given != "" && slices.ContainsFunc(k.Answers, func(a string) bool { return NormaliseShort(a) == given })
}

// GradableQuestion is a question as stored. CorrectAnswer comes straight from the database
// (and AI-generated rows could carry anything), so its shape is checked rather than trusted.
//
// Grading runs on the server because the correct answer never leaves it: the browser is not
// granted the correct_answer column at all.
type GradableQuestion struct {
	QuestionType  QuestionType
	CorrectAnswer jsontext.Value
}

// GivenAnswer is what the learner sent: SelectedOptions (non-nil, possibly empty) for choice
// questions, Value for numeric ones, Text for short ones.
type GivenAnswer struct {
	SelectedOptions []int    `json:"selectedOptions,omitempty"`
	Value           *float64 `json:"value,omitempty"`
	Text            *string  `json:"text,omitempty"`
}

// maxSafeInt is JavaScript's Number.MAX_SAFE_INTEGER, the bound zod's int() enforces.
const maxSafeInt = 1<<53 - 1

func malformed(detail string) error {
	return httpx.E(http.StatusInternalServerError, "QUESTION_MALFORMED", "This question cannot be graded: "+detail)
}

// GradeAnswer decides whether an answer is right. Pure: same inputs, same result. A short answer
// that matches no accepted answer returns ErrNeedsJudgement: the caller grades it on the rubric.
func GradeAnswer(q GradableQuestion, given GivenAnswer) (isCorrect bool, err error) {
	switch q.QuestionType {
	case MCQ, MSQ:
		if given.SelectedOptions == nil {
			return false, httpx.E(http.StatusBadRequest, "INVALID_ANSWER", "This question needs selected options.")
		}
		// Stored shape (003_questions.sql): a non-empty list of option indexes.
		var expected []float64
		if json.Unmarshal(q.CorrectAnswer, &expected) != nil || len(expected) == 0 {
			return false, malformed("its stored answer is not a list of option indexes")
		}
		for _, v := range expected {
			if v != math.Trunc(v) || v < 0 || v > maxSafeInt {
				return false, malformed("its stored answer is not a list of option indexes")
			}
		}
		if q.QuestionType == MCQ && len(expected) != 1 {
			return false, malformed("a single-answer question has more than one answer")
		}

		// Set comparison, so order does not matter and duplicates cannot pad a match.
		// No partial credit for msq: two of three correct options is not knowing the concept,
		// and half marks would flatter the mastery score.
		chosen := map[float64]bool{}
		for _, o := range given.SelectedOptions {
			chosen[float64(o)] = true
		}
		correct := map[float64]bool{}
		for _, o := range expected {
			correct[o] = true
		}
		if len(chosen) != len(correct) {
			return false, nil
		}
		for o := range correct {
			if !chosen[o] {
				return false, nil
			}
		}
		return true, nil

	case Numeric:
		if given.Value == nil {
			return false, httpx.E(http.StatusBadRequest, "INVALID_ANSWER", "This question needs a numeric value.")
		}
		var stored struct {
			Value *float64 `json:"value"`
			Tol   *float64 `json:"tol"`
		}
		if json.Unmarshal(q.CorrectAnswer, &stored) != nil || stored.Value == nil || stored.Tol == nil || *stored.Tol < 0 {
			return false, malformed("its stored answer is not a value with a tolerance")
		}
		// Tolerance rather than equality: 1/3 typed as 0.33 is a right answer.
		return math.Abs(*given.Value-*stored.Value) <= *stored.Tol, nil

	case Short:
		if given.Text == nil {
			return false, httpx.E(http.StatusBadRequest, "INVALID_ANSWER", "This question needs a typed answer.")
		}
		key, err := ParseShortKey(q.CorrectAnswer)
		if err != nil {
			return false, err
		}
		if MatchesShort(key, *given.Text) {
			return true, nil
		}
		// Nothing typed is wrong without asking anybody.
		if NormaliseShort(*given.Text) == "" {
			return false, nil
		}
		return false, ErrNeedsJudgement

	default:
		return false, malformed("unknown question type " + string(q.QuestionType))
	}
}
