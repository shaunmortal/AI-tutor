package practice

// Request validation, reproducing the TS zod schemas: unknown keys are ignored (zod strips
// them), fields are checked in schema order, and the first problem becomes
// 400 INVALID_INPUT "path: message" with zod v4's wording.

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// zod v4's z.string().uuid().
var uuidRe = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

func isUUID(s string) bool { return uuidRe.MatchString(s) }

func invalid(path, msg string) error {
	if path != "" {
		msg = path + ": " + msg
	}
	return httpx.E(http.StatusBadRequest, "INVALID_INPUT", msg)
}

// kindOf names a JSON value the way zod's "received ..." does.
func kindOf(v any, present bool) string {
	switch v.(type) {
	case nil:
		if !present {
			return "undefined"
		}
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	}
	return "object"
}

func expected(path, want string, v any, present bool) error {
	return invalid(path, "Invalid input: expected "+want+", received "+kindOf(v, present))
}

// readObject reads the JSON body as an object. An empty body is "undefined", as Express 5
// leaves req.body when nothing was parsed.
func readObject(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 100<<10))
	if err != nil {
		return nil, httpx.E(http.StatusBadRequest, "INVALID_INPUT", "Invalid request body.")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, expected("", "object", nil, false)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, httpx.E(http.StatusBadRequest, "INVALID_INPUT", "Invalid request body.")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, expected("", "object", v, true)
	}
	return m, nil
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// intIn is z.number().int().min(lo).max(hi) on a present value.
func intIn(path string, v any, lo, hi float64) (int, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, expected(path, "number", v, true)
	}
	if f != math.Trunc(f) {
		return 0, invalid(path, "Invalid input: expected int, received number")
	}
	if f < lo {
		return 0, invalid(path, "Too small: expected number to be >="+num(lo))
	}
	if f > hi {
		return 0, invalid(path, "Too big: expected number to be <="+num(hi))
	}
	return int(f), nil
}

// optInt is intIn(...).optional(): nil when absent.
func optInt(m map[string]any, key string, lo, hi float64) (*int, error) {
	v, present := m[key]
	if !present {
		return nil, nil
	}
	n, err := intIn(key, v, lo, hi)
	return &n, err
}

func uuidField(path string, v any, present bool) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", expected(path, "string", v, present)
	}
	if !isUUID(s) {
		return "", invalid(path, "Invalid UUID")
	}
	return s, nil
}

// intList is z.array(z.number().int().min(0).max(hi)), with .max(maxLen) when maxLen >= 0.
// The result is never nil, so an empty selection stays distinguishable from none.
func intList(path string, v any, hi float64, maxLen int) ([]int, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, expected(path, "array", v, true)
	}
	out := make([]int, len(list))
	for i, item := range list {
		n, err := intIn(path+"."+strconv.Itoa(i), item, 0, hi)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	if maxLen >= 0 && len(list) > maxLen {
		return nil, invalid(path, "Too big: expected array to have <="+strconv.Itoa(maxLen)+" items")
	}
	return out, nil
}

// enumField is z.enum(options).default(def).
func enumField(m map[string]any, key, def string, options ...string) (string, error) {
	v, present := m[key]
	if !present {
		return def, nil
	}
	if s, ok := v.(string); ok {
		for _, o := range options {
			if s == o {
				return s, nil
			}
		}
	}
	return "", invalid(key, `Invalid option: expected one of "`+strings.Join(options, `"|"`)+`"`)
}

// givenAnswer is what the learner sent: Options non-nil for choice questions, else Value.
type givenAnswer struct {
	Options []int
	Value   *float64
}

// json is the answer as recorded in question_attempts.given_answer.
func (a givenAnswer) json() json.RawMessage {
	var raw []byte
	if a.Options != nil {
		raw, _ = json.Marshal(map[string]any{"selectedOptions": a.Options})
	} else {
		raw, _ = json.Marshal(map[string]any{"value": a.Value})
	}
	return raw
}

// --- schemas ---

type attemptInput struct {
	QuestionID       string
	Answer           givenAnswer
	TimeTakenSeconds *int
	Source           string
}

func parseAttempt(w http.ResponseWriter, r *http.Request) (attemptInput, error) {
	var in attemptInput
	m, err := readObject(w, r)
	if err != nil {
		return in, err
	}
	v, present := m["questionId"]
	if in.QuestionID, err = uuidField("questionId", v, present); err != nil {
		return in, err
	}
	if v, present := m["selectedOptions"]; present {
		if in.Answer.Options, err = intList("selectedOptions", v, 50, 20); err != nil {
			return in, err
		}
	}
	if v, present := m["value"]; present {
		f, ok := v.(float64)
		if !ok {
			return in, expected("value", "number", v, true)
		}
		in.Answer.Value = &f
	}
	if v := m["timeTakenSeconds"]; v != nil { // absent or null both mean null
		n, err := intIn("timeTakenSeconds", v, 0, 3600)
		if err != nil {
			return in, err
		}
		in.TimeTakenSeconds = &n
	}
	if in.Source, err = enumField(m, "source", "practice",
		"practice", "assessment", "topic_test", "mock_test", "revision"); err != nil {
		return in, err
	}
	if (in.Answer.Options == nil) == (in.Answer.Value == nil) {
		return in, invalid("", "Send either selectedOptions (choice questions) or value (numeric questions), not both.")
	}
	return in, nil
}

type paperAnswer struct {
	QuestionID string
	Answer     *givenAnswer // nil when neither selectedOptions nor value was sent
}

// parseAnswers is the shared `answers` array of the diagnostic and mock test submissions.
func parseAnswers(m map[string]any, maxLen int) ([]paperAnswer, error) {
	v, present := m["answers"]
	list, ok := v.([]any)
	if !ok {
		return nil, expected("answers", "array", v, present)
	}
	out := make([]paperAnswer, len(list))
	for i, item := range list {
		path := "answers." + strconv.Itoa(i)
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, expected(path, "object", item, true)
		}
		qv, present := obj["questionId"]
		id, err := uuidField(path+".questionId", qv, present)
		if err != nil {
			return nil, err
		}
		out[i].QuestionID = id
		var a givenAnswer
		if sv, present := obj["selectedOptions"]; present {
			if a.Options, err = intList(path+".selectedOptions", sv, 20, -1); err != nil {
				return nil, err
			}
		}
		if vv, present := obj["value"]; present {
			f, ok := vv.(float64)
			if !ok {
				return nil, expected(path+".value", "number", vv, true)
			}
			a.Value = &f
		}
		// selectedOptions wins when both are sent, as in the TS mapping.
		if a.Options != nil {
			out[i].Answer = &givenAnswer{Options: a.Options}
		} else if a.Value != nil {
			out[i].Answer = &givenAnswer{Value: a.Value}
		}
	}
	if len(list) > maxLen {
		return nil, invalid("answers", "Too big: expected array to have <="+strconv.Itoa(maxLen)+" items")
	}
	return out, nil
}

// answersByQuestion is JS `new Map(answers.map(...))`: the last entry for a question wins.
func answersByQuestion(answers []paperAnswer) map[string]*givenAnswer {
	out := make(map[string]*givenAnswer, len(answers))
	for _, a := range answers {
		out[a.QuestionID] = a.Answer
	}
	return out
}
