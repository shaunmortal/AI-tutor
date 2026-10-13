// Package validate is where AI output stops being AI output and becomes data.
//
// Two gates, in order: the schema (is it the shape we asked for) and the business rules (is
// it a usable question). The second is the one that matters: five identical options, an
// answer index past the end of the list, an explanation that repeats the question, "see the
// diagram above" — none of those are malformed JSON, and all would reach a learner if only the
// shape were checked.
package validate

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// GeneratedQuestion is one question the model wrote, after the schema gate.
type GeneratedQuestion struct {
	QuestionType         string   `json:"questionType"` // "mcq" or "msq"
	Body                 string   `json:"body"`
	Options              []string `json:"options"`
	CorrectOptionIndexes []int    `json:"correctOptionIndexes"`
	Explanation          string   `json:"explanation"`
	Difficulty           string   `json:"difficulty"` // "easy", "medium" or "hard"
}

// Failure is a rejected question. Index is its position in the batch, or -1 when the whole
// reply failed the schema.
type Failure struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// Outcome is a batch split into usable and rejected questions.
type Outcome struct {
	Accepted []GeneratedQuestion `json:"accepted"`
	Rejected []Failure           `json:"rejected"`
}

// danglingReferences mean the question depends on something the learner cannot see.
var danglingReferences = []string{
	"the above",
	"shown above",
	"the following diagram",
	"the figure",
	"the image",
	"previous question",
	"as discussed",
}

// BusinessCheck applies the per-question rules. It returns "" when the question is usable, or
// the reason it is not.
func BusinessCheck(q GeneratedQuestion) string {
	idx := q.CorrectOptionIndexes

	// A single-answer question with two answers is a broken question, not a hard one.
	if q.QuestionType == "mcq" && len(idx) != 1 {
		return "single-answer question has more than one correct option"
	}
	if q.QuestionType == "msq" && len(idx) < 2 {
		return "multi-answer question has fewer than two correct options"
	}
	// The check that stops a learner being marked wrong for the right answer.
	if slices.ContainsFunc(idx, func(i int) bool { return i >= len(q.Options) }) {
		return "correct answer points at an option that does not exist"
	}
	if distinct(idx) != len(idx) {
		return "the same option is listed as correct twice"
	}
	if len(idx) == len(q.Options) {
		return "every option is marked correct"
	}

	trimmed := make([]string, len(q.Options))
	for i, o := range q.Options {
		trimmed[i] = fold(o)
	}
	if distinct(trimmed) != len(trimmed) {
		return "two options are identical"
	}
	// "All of the above" breaks when options get shuffled, which the UI is free to do.
	if slices.ContainsFunc(trimmed, func(o string) bool { return strings.Contains(o, "of the above") }) {
		return "options refer to their own ordering"
	}

	lowerBody := strings.ToLower(q.Body)
	for _, phrase := range danglingReferences {
		if strings.Contains(lowerBody, phrase) {
			return fmt.Sprintf("question refers to something the learner cannot see (%q)", phrase)
		}
	}
	// An explanation that is just the question again teaches nothing.
	if fold(q.Explanation) == strings.TrimFunc(lowerBody, isJSSpace) {
		return "explanation only repeats the question"
	}
	// A correct option quoted verbatim in the question gives the answer away.
	for _, i := range idx {
		if text := trimmed[i]; jsLen(text) > 12 && strings.Contains(lowerBody, text) {
			return "the question text contains the correct answer"
		}
	}
	return ""
}

// ValidateGeneratedQuestions runs both gates over a raw provider reply.
//
// A batch is never rejected wholesale for one bad question: four usable questions and one dud
// is the normal case, not an error.
func ValidateGeneratedQuestions(raw []byte) Outcome {
	out := Outcome{Accepted: []GeneratedQuestion{}, Rejected: []Failure{}}

	questions, err := parseQuestions(raw)
	if err != nil {
		out.Rejected = append(out.Rejected, Failure{Index: -1, Reason: "reply did not match the expected shape: " + err.Error()})
		return out
	}

	seenBodies := map[string]bool{}
	for i, q := range questions {
		if problem := BusinessCheck(q); problem != "" {
			out.Rejected = append(out.Rejected, Failure{Index: i, Reason: problem})
			continue
		}
		// The model occasionally repeats itself inside one batch.
		fp := fold(q.Body)
		if seenBodies[fp] {
			out.Rejected = append(out.Rejected, Failure{Index: i, Reason: "duplicate of another question in the batch"})
			continue
		}
		seenBodies[fp] = true
		out.Accepted = append(out.Accepted, q)
	}
	return out
}

// AnswerCheck is the independent solver's verdict on a generated question.
type AnswerCheck struct {
	CorrectOptionIndexes []int  `json:"correctOptionIndexes"`
	Confidence           string `json:"confidence"` // "low", "medium" or "high"
	Unanswerable         bool   `json:"unanswerable"`
	Note                 string `json:"note,omitempty"`
}

// ParseAnswerCheck is the schema gate for the answer checker's reply.
func ParseAnswerCheck(raw []byte) (AnswerCheck, error) {
	var w struct {
		CorrectOptionIndexes *[]*float64    `json:"correctOptionIndexes"`
		Confidence           *string        `json:"confidence"`
		Unanswerable         *bool          `json:"unanswerable"`
		Note                 jsontext.Value `json:"note"`
	}
	if err := decode(raw, &w); err != nil {
		return AnswerCheck{}, err
	}
	var c AnswerCheck
	var err error
	if c.CorrectOptionIndexes, err = indexes("correctOptionIndexes", w.CorrectOptionIndexes); err != nil {
		return c, err
	}
	if len(c.CorrectOptionIndexes) > 6 {
		return c, errors.New("correctOptionIndexes too big: expected array to have <=6 items")
	}
	if c.Confidence, err = enum("confidence", w.Confidence, "low", "medium", "high"); err != nil {
		return c, err
	}
	if w.Unanswerable == nil {
		return c, errors.New("unanswerable invalid input: expected boolean")
	}
	c.Unanswerable = *w.Unanswerable
	// Optional: absent is fine, but null is not (zod's optional() accepts undefined only).
	if len(w.Note) > 0 {
		var note *string
		if json.Unmarshal(w.Note, &note) != nil || note == nil {
			return c, errors.New("note invalid input: expected string")
		}
		if jsLen(*note) > 1000 {
			return c, errors.New("note too big: expected string to have <=1000 characters")
		}
		c.Note = *note
	}
	return c, nil
}

// AgreesWithProposedAnswer decides whether a generated question may be shown to learners.
//
// The bar: a second, independent request solved it without seeing the proposed answer, agreed,
// was confident, and did not flag it as ambiguous. Anything less and the question stays hidden
// (not deleted, so it can be reviewed) — no learner sees a question whose answer we doubt.
func AgreesWithProposedAnswer(proposed []int, check AnswerCheck) (verified bool, reason string) {
	if check.Unanswerable {
		return false, "checker judged the question unanswerable"
	}
	if check.Confidence == "low" {
		return false, "checker was not confident in any answer"
	}
	a, b := sortedSet(proposed), sortedSet(check.CorrectOptionIndexes)
	if a != b {
		return false, fmt.Sprintf("checker answered [%s] but the question claims [%s]", b, a)
	}
	return true, "independent check agreed with the answer"
}

func sortedSet(xs []int) string {
	s := slices.Clone(xs)
	slices.Sort(s)
	s = slices.Compact(s)
	parts := make([]string, len(s))
	for i, x := range s {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

// parseQuestions is the schema gate for a batch: {questions: [1..20 questions]}.
func parseQuestions(raw []byte) ([]GeneratedQuestion, error) {
	type wireQuestion struct {
		QuestionType         *string     `json:"questionType"`
		Body                 *string     `json:"body"`
		Options              *[]*string  `json:"options"`
		CorrectOptionIndexes *[]*float64 `json:"correctOptionIndexes"`
		Explanation          *string     `json:"explanation"`
		Difficulty           *string     `json:"difficulty"`
	}
	var w struct {
		Questions *[]wireQuestion `json:"questions"`
	}
	if err := decode(raw, &w); err != nil {
		return nil, err
	}
	if w.Questions == nil {
		return nil, errors.New("questions invalid input: expected array")
	}

	var out []GeneratedQuestion
	for i, wq := range *w.Questions {
		p := fmt.Sprintf("questions.%d.", i)
		var q GeneratedQuestion
		var err error
		if q.QuestionType, err = enum(p+"questionType", wq.QuestionType, "mcq", "msq"); err != nil {
			return nil, err
		}
		if q.Body, err = str(p+"body", wq.Body, 15, 2000); err != nil {
			return nil, err
		}
		if wq.Options == nil {
			return nil, errors.New(p + "options invalid input: expected array")
		}
		for j, o := range *wq.Options {
			opt, err := str(fmt.Sprintf("%soptions.%d", p, j), o, 1, 500)
			if err != nil {
				return nil, err
			}
			q.Options = append(q.Options, opt)
		}
		if n := len(q.Options); n < 2 || n > 6 {
			return nil, fmt.Errorf("%soptions too %s: expected array to have %s items", p, bigOrSmall(n < 2), bounds(n < 2, 2, 6))
		}
		if q.CorrectOptionIndexes, err = indexes(p+"correctOptionIndexes", wq.CorrectOptionIndexes); err != nil {
			return nil, err
		}
		if len(q.CorrectOptionIndexes) < 1 {
			return nil, errors.New(p + "correctOptionIndexes too small: expected array to have >=1 items")
		}
		if q.Explanation, err = str(p+"explanation", wq.Explanation, 20, 2000); err != nil {
			return nil, err
		}
		if q.Difficulty, err = enum(p+"difficulty", wq.Difficulty, "easy", "medium", "hard"); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if n := len(out); n < 1 || n > 20 {
		return nil, fmt.Errorf("questions too %s: expected array to have %s items", bigOrSmall(n < 1), bounds(n < 1, 1, 20))
	}
	return out, nil
}

// decode unmarshals like JSON.parse would feed zod: case-sensitive keys, unknown keys
// ignored, duplicate keys allowed (last wins). A type mismatch is reported at its path.
func decode(raw []byte, v any) error {
	if strings.TrimSpace(string(raw)) == "null" {
		return errors.New(" invalid input: expected object, received null")
	}
	err := json.Unmarshal(raw, v, jsontext.AllowDuplicateNames(true))
	var se *json.SemanticError
	if errors.As(err, &se) {
		path := strings.ReplaceAll(strings.TrimPrefix(string(se.JSONPointer), "/"), "/", ".")
		return fmt.Errorf("%s invalid input: received %s", path, se.JSONKind)
	}
	if err != nil {
		return fmt.Errorf(" invalid JSON: %w", err)
	}
	return nil
}

func str(path string, p *string, minLen, maxLen int) (string, error) {
	if p == nil {
		return "", errors.New(path + " invalid input: expected string")
	}
	if n := jsLen(*p); n < minLen || n > maxLen {
		return "", fmt.Errorf("%s too %s: expected string to have %s characters", path, bigOrSmall(n < minLen), bounds(n < minLen, minLen, maxLen))
	}
	return *p, nil
}

func enum(path string, p *string, allowed ...string) (string, error) {
	if p == nil || !slices.Contains(allowed, *p) {
		return "", fmt.Errorf("%s invalid option: expected one of %q", path, allowed)
	}
	return *p, nil
}

// indexes checks z.array(z.number().int().min(0)): JSON numbers like 1.0 are integers too.
func indexes(path string, p *[]*float64) ([]int, error) {
	if p == nil {
		return nil, errors.New(path + " invalid input: expected array")
	}
	out := []int{}
	for i, f := range *p {
		switch {
		case f == nil:
			return nil, fmt.Errorf("%s.%d invalid input: expected number", path, i)
		case *f != math.Trunc(*f) || math.Abs(*f) > 1<<53-1:
			return nil, fmt.Errorf("%s.%d invalid input: expected int", path, i)
		case *f < 0:
			return nil, fmt.Errorf("%s.%d too small: expected number to be >=0", path, i)
		}
		out = append(out, int(*f))
	}
	return out, nil
}

func bigOrSmall(small bool) string {
	if small {
		return "small"
	}
	return "big"
}

func bounds(small bool, lo, hi int) string {
	if small {
		return ">=" + strconv.Itoa(lo)
	}
	return "<=" + strconv.Itoa(hi)
}

func distinct[T comparable](xs []T) int {
	seen := map[T]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return len(seen)
}

// fold is JavaScript's s.trim().toLowerCase().
func fold(s string) string { return strings.ToLower(strings.TrimFunc(s, isJSSpace)) }

// isJSSpace matches what JavaScript's trim() strips, which is wider than unicode.IsSpace.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// jsLen is a string's JavaScript length in UTF-16 code units, which is what zod measures.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
