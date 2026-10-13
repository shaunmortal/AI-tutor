package learning

// Request validation that answers exactly as the TS zod schemas did: the first failing field in
// schema order, as 400 INVALID_INPUT "field: zod's message". Unknown keys are ignored (zod
// strips them), unlike httpx.Decode, which rejects them.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// zod v4's z.string().uuid(): RFC 9562 versions 1-8, plus the nil and max UUIDs.
var uuidRe = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

func isUUID(s string) bool { return uuidRe.MatchString(s) }

func invalid(msg string) error { return httpx.E(http.StatusBadRequest, "INVALID_INPUT", msg) }

// fields is a decoded JSON object body.
type fields map[string]any

// readObject reads the body as a JSON object. An empty body is zod's `undefined`.
func readObject(w http.ResponseWriter, r *http.Request) (fields, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 100<<10))
	if err != nil {
		return nil, invalid("Invalid request body: " + err.Error())
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalid("Invalid input: expected object, received undefined")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, invalid("Invalid request body: " + err.Error())
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, invalid("Invalid request body: unexpected data after the JSON value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, invalid("Invalid input: expected object, received " + kindOf(v, true))
	}
	return m, nil
}

func kindOf(v any, present bool) string {
	switch v.(type) {
	case nil:
		if !present {
			return "undefined"
		}
		return "null"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	}
	return "object"
}

func expected(key, want string, v any, present bool) error {
	return invalid(fmt.Sprintf("%s: Invalid input: expected %s, received %s", key, want, kindOf(v, present)))
}

// str is z.string().trim().min(min).max(max) on f[key]; lengths are UTF-16 units as in JS.
func (f fields) str(key string, min, max int) (string, error) {
	v, present := f[key]
	s, ok := v.(string)
	if !ok {
		return "", expected(key, "string", v, present)
	}
	s = jsTrim(s)
	return s, lengthCheck(key, "string", "characters", jsLen(s), min, max)
}

// optStr is str(...).optional(): absent means nil.
func (f fields) optStr(key string, min, max int) (*string, error) {
	if _, present := f[key]; !present {
		return nil, nil
	}
	s, err := f.str(key, min, max)
	return &s, err
}

func (f fields) uuid(key string) (string, error) {
	v, present := f[key]
	s, ok := v.(string)
	if !ok {
		return "", expected(key, "string", v, present)
	}
	if !isUUID(s) {
		return "", invalid(key + ": Invalid UUID")
	}
	return s, nil
}

// int is z.number().int().min(min).max(max).
func (f fields) int(key string, min, max int) (int, error) {
	v, present := f[key]
	n, ok := v.(json.Number)
	if !ok {
		return 0, expected(key, "number", v, present)
	}
	x, err := strconv.ParseFloat(string(n), 64)
	const maxSafe = 1<<53 - 1
	switch {
	case err != nil || math.Trunc(x) != x:
		return 0, invalid(key + ": Invalid input: expected int, received number")
	case x > maxSafe:
		return 0, invalid(fmt.Sprintf("%s: Too big: expected int to be <=%d", key, maxSafe))
	case x < -maxSafe:
		return 0, invalid(fmt.Sprintf("%s: Too small: expected int to be >=%d", key, -maxSafe))
	case x < float64(min):
		return 0, invalid(fmt.Sprintf("%s: Too small: expected number to be >=%d", key, min))
	case x > float64(max):
		return 0, invalid(fmt.Sprintf("%s: Too big: expected number to be <=%d", key, max))
	}
	return int(x), nil
}

// uuids is z.array(z.string().uuid()).min(min).max(max): elements are checked before length.
func (f fields) uuids(key string, min, max int) ([]string, error) {
	v, present := f[key]
	arr, ok := v.([]any)
	if !ok {
		return nil, expected(key, "array", v, present)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		at := key + "." + strconv.Itoa(i)
		s, ok := e.(string)
		if !ok {
			return nil, expected(at, "string", e, true)
		}
		if !isUUID(s) {
			return nil, invalid(at + ": Invalid UUID")
		}
		out[i] = s
	}
	return out, lengthCheck(key, "array", "items", len(arr), min, max)
}

func lengthCheck(key, what, unit string, n, min, max int) error {
	if n < min {
		return invalid(fmt.Sprintf("%s: Too small: expected %s to have >=%d %s", key, what, min, unit))
	}
	if n > max {
		return invalid(fmt.Sprintf("%s: Too big: expected %s to have <=%d %s", key, what, max, unit))
	}
	return nil
}

// jsTrim is String.prototype.trim: Unicode white space plus the BOM, but not NEL.
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\uFEFF' || (unicode.IsSpace(r) && r != '\u0085') })
}

func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }
