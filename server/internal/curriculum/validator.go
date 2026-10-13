// Package curriculum is where the AI's answer about a study goal stops being an answer and
// becomes a syllabus.
//
// Two gates, as everywhere AI output is used: the schema check proves the reply has the right
// shape, and the business check proves it is a usable syllabus. The second matters more — one
// subject with forty topics, a topic listed twice, a subject called "Subject 1", a weight of 900
// are all valid JSON and all visible to a learner if only the shape were checked.
package curriculum

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Limits chosen so a syllabus is big enough to be real and small enough to be plannable.
const (
	MinSubjects         = 1
	MaxSubjects         = 12
	MinTopicsPerSubject = 2
	MaxTopicsPerSubject = 15
	// Beyond this a day-by-day plan spreads too thin to mean anything.
	MaxTotalTopics = 90
	MinWeight      = 0.5
	MaxWeight      = 3.0
)

const (
	// Longest description worth storing. Past this it is prose, not a summary.
	maxDescription = 2000
	// How much of the model's reason to show a learner: a reason, not a lecture.
	maxShownReason = 240
)

// reply is the model's answer, after the schema gate.
//
// Strict about structure, deliberately loose about contents: contents are trimmed and dropped
// by the business rules, not rejected here. A tight cap on reason once turned a correct refusal
// ("apple" is not something you study) into "something went wrong" because the explanation
// overflowed. The caps below are bounds against a pathological reply, not judgements of a good one.
type reply struct {
	IsStudyGoal bool           `json:"isStudyGoal"`
	Reason      string         `json:"reason"`
	Name        string         `json:"name"`
	Slug        string         `json:"slug"`
	Description string         `json:"description"`
	Subjects    []replySubject `json:"subjects"`
}

// replySubject is one subject as the model proposed it.
type replySubject struct {
	Name string `json:"name"`
	// Weight is any JSON value: a model being loose about one number is not a reason to discard
	// the syllabus, so it is coerced and clamped later instead of type-checked here.
	Weight jsontext.Value `json:"weight"`
	Topics []string       `json:"topics"`
}

// Resolved is a validated, normalised syllabus ready to insert.
type Resolved struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Subjects    []ResolvedSubject `json:"subjects"`
	// CourseMap marks a syllabus built from one learner's material: not shared (migration 031).
	CourseMap bool `json:"-"`
}

// ResolvedSubject is a subject that survived validation.
type ResolvedSubject struct {
	Name   string   `json:"name"`
	Weight float64  `json:"weight"`
	Topics []string `json:"topics"`
}

// Outcome is either a usable curriculum (OK) or a refusal. LearnerMessage is shown to the
// learner; LogDetail explains it to us.
type Outcome struct {
	OK             bool
	Curriculum     Resolved
	LearnerMessage string
	LogDetail      string
}

// parseReply is the schema gate. Every field is required and non-null except weight.
func parseReply(raw []byte) (reply, error) {
	type wireSubject struct {
		Name   *string        `json:"name"`
		Weight jsontext.Value `json:"weight"`
		Topics *[]*string     `json:"topics"`
	}
	var w struct {
		IsStudyGoal *bool          `json:"isStudyGoal"`
		Reason      *string        `json:"reason"`
		Name        *string        `json:"name"`
		Slug        *string        `json:"slug"`
		Description *string        `json:"description"`
		Subjects    *[]wireSubject `json:"subjects"`
	}
	if err := json.Unmarshal(raw, &w, jsontext.AllowDuplicateNames(true)); err != nil {
		var se *json.SemanticError
		if errors.As(err, &se) {
			return reply{}, fmt.Errorf("%s invalid input: received %s", pointerPath(se.JSONPointer), se.JSONKind)
		}
		return reply{}, fmt.Errorf(" invalid JSON: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return reply{}, errors.New(" invalid input: expected object, received null")
	}

	str := func(path string, p *string, max int) (string, error) {
		if p == nil {
			return "", fmt.Errorf("%s invalid input: expected string", path)
		}
		if jsLen(*p) > max {
			return "", fmt.Errorf("%s too big: expected string to have <=%d characters", path, max)
		}
		return *p, nil
	}
	var r reply
	var err error
	if w.IsStudyGoal == nil {
		return r, errors.New("isStudyGoal invalid input: expected boolean")
	}
	r.IsStudyGoal = *w.IsStudyGoal
	if r.Reason, err = str("reason", w.Reason, 4000); err != nil {
		return r, err
	}
	if r.Name, err = str("name", w.Name, 2000); err != nil {
		return r, err
	}
	if r.Slug, err = str("slug", w.Slug, 2000); err != nil {
		return r, err
	}
	if r.Description, err = str("description", w.Description, 20_000); err != nil {
		return r, err
	}
	if w.Subjects == nil {
		return r, errors.New("subjects invalid input: expected array")
	}
	for i, s := range *w.Subjects {
		path := fmt.Sprintf("subjects.%d", i)
		sub := replySubject{Weight: s.Weight}
		if sub.Name, err = str(path+".name", s.Name, 2000); err != nil {
			return r, err
		}
		if s.Topics == nil {
			return r, fmt.Errorf("%s.topics invalid input: expected array", path)
		}
		for j, t := range *s.Topics {
			topic, err := str(fmt.Sprintf("%s.topics.%d", path, j), t, 2000)
			if err != nil {
				return r, err
			}
			sub.Topics = append(sub.Topics, topic)
		}
		if len(*s.Topics) > 400 {
			return r, fmt.Errorf("%s.topics too big: expected array to have <=400 items", path)
		}
		r.Subjects = append(r.Subjects, sub)
	}
	if len(*w.Subjects) > 200 {
		return r, errors.New("subjects too big: expected array to have <=200 items")
	}
	return r, nil
}

// ValidateReply validates the model's raw JSON reply and normalises it into something
// insertable. "That is not a study goal" is a normal outcome, so it is returned rather than
// raised as an error.
func ValidateReply(raw []byte, originalGoalText string) Outcome {
	rep, err := parseReply(raw)
	if err != nil {
		return Outcome{
			LearnerMessage: "Something went wrong working out that goal. Try rewording it.",
			LogDetail:      "reply failed the schema: " + err.Error(),
		}
	}

	// The model's own verdict comes first: it saw the text and we did not.
	if !rep.IsStudyGoal {
		detail := shorten(tidy(rep.Reason), maxShownReason)
		if detail == "" {
			return Outcome{
				LearnerMessage: fmt.Sprintf(`"%s" does not look like something to study. Try an exam, a class, or a subject — for example "class 10", "GATE CSE", or "learn python".`, jsSlice(tidy(originalGoalText), 60)),
				LogDetail:      "model rejected the goal: no reason given",
			}
		}
		return Outcome{
			LearnerMessage: "That does not look like something to study. " + detail,
			LogDetail:      "model rejected the goal: " + detail,
		}
	}

	name := tidy(rep.Name)
	description := shorten(tidy(rep.Description), maxDescription)

	if jsLen(name) < 2 || looksLikeProse(name) {
		return Outcome{
			LearnerMessage: "Could not work out a clear syllabus for that. Try naming the exam or class directly.",
			LogDetail:      "curriculum name unusable: " + quote(rep.Name),
		}
	}

	// The model's slug is a suggestion; ours must satisfy the database's shape constraint, so it
	// is regenerated. Falling back to the learner's text keeps a key when the model's is unusable.
	slug := ToSlug(rep.Slug)
	if slug == "" {
		slug = ToSlug(name)
	}
	if slug == "" {
		slug = ToSlug(originalGoalText)
	}
	if len(slug) < 2 {
		return Outcome{
			LearnerMessage: "Could not work out a syllabus for that. Try rewording it.",
			LogDetail:      "no usable slug from " + quote(rep.Slug),
		}
	}

	subjects := []ResolvedSubject{}
	seenSubjects := map[string]bool{}
	totalTopics := 0

	for _, subject := range rep.Subjects {
		subjectName := tidy(subject.Name)
		if jsLen(subjectName) < 2 || looksLikeProse(subjectName) {
			continue
		}
		// Duplicate subject names would violate the topics table's uniqueness constraint.
		key := strings.ToLower(subjectName)
		if seenSubjects[key] {
			continue
		}

		seenTopics := map[string]bool{}
		topics := []string{}
		for _, raw := range subject.Topics {
			topic := tidy(raw)
			if jsLen(topic) < 2 || looksLikeProse(topic) {
				continue
			}
			topicKey := strings.ToLower(topic)
			if seenTopics[topicKey] {
				continue
			}
			seenTopics[topicKey] = true
			topics = append(topics, topic)
			if len(topics) >= MaxTopicsPerSubject {
				break
			}
		}

		// A subject with one topic is a topic that ended up a level too high.
		if len(topics) < MinTopicsPerSubject {
			continue
		}
		if totalTopics+len(topics) > MaxTotalTopics {
			break
		}

		seenSubjects[key] = true
		totalTopics += len(topics)

		// Clamped rather than rejected: a weight out of range is the model being loose about a
		// number. A missing, zero or unparseable weight (NaN) falls back to 1.
		weight := jsNumber(subject.Weight)
		if weight == 0 || math.IsNaN(weight) {
			weight = 1
		}
		subjects = append(subjects, ResolvedSubject{
			Name:   subjectName,
			Weight: math.Min(MaxWeight, math.Max(MinWeight, weight)),
			Topics: topics,
		})
		if len(subjects) >= MaxSubjects {
			break
		}
	}

	if len(subjects) < MinSubjects {
		return Outcome{
			LearnerMessage: `Could not build a usable syllabus for that. Try being more specific — for example "class 10 science" rather than "science".`,
			LogDetail:      fmt.Sprintf("no subject survived validation (model returned %d)", len(rep.Subjects)),
		}
	}

	return Outcome{OK: true, Curriculum: Resolved{
		Slug: slug, Name: name, Description: jsSlice(description, 500), Subjects: subjects,
	}}
}

var (
	nonSlug     = regexp.MustCompile(`[^a-z0-9]+`)
	edgeHyphens = regexp.MustCompile(`^-+|-+$`)
)

// ToSlug turns free text into a lookup key. Used on the way in as well: the incoming goal is
// slugged to check whether the syllabus already exists, so a repeated goal costs nothing and
// two learners typing the same thing share one syllabus.
func ToSlug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		// Drop accents, so "mathématiques" and "mathematiques" are one key.
		if r >= 0x300 && r <= 0x36f {
			continue
		}
		if f, ok := accentFold[r]; ok {
			b.WriteString(f)
		} else {
			b.WriteRune(r)
		}
	}
	s := edgeHyphens.ReplaceAllString(nonSlug.ReplaceAllString(b.String(), "-"), "")
	if len(s) > 80 {
		s = s[:80] // all ASCII by now
	}
	return strings.TrimRight(s, "-")
}

// accentFold is what NFKD followed by stripping combining marks does to lowercase Latin-1 and
// Latin Extended-A letters. Letters with no decomposition (æ, ø, ß, đ, ł…) are absent and become
// hyphens, as they do in the TS.
// ponytail: covers Latin-1/Extended-A only; Vietnamese, ligatures beyond ĳ, fullwidth forms etc.
// slug differently from the TS NFKD. Swap in golang.org/x/text/unicode/norm if those matter.
var accentFold = func() map[rune]string {
	m := map[rune]string{'ĳ': "ij", 'ſ': "s", 'ŀ': "l·", 'ŉ': "ʼn"}
	for base, letters := range map[string]string{
		"a": "àáâãäåāăą", "c": "çćĉċč", "d": "ď", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥ",
		"i": "ìíîïĩīĭį", "j": "ĵ", "k": "ķ", "l": "ĺļľ", "n": "ñńņň", "o": "òóôõöōŏő",
		"r": "ŕŗř", "s": "śŝşš", "t": "ţť", "u": "ùúûüũūŭůűų", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range letters {
			m[r] = base
		}
	}
	return m
}()

// jsSpace is JavaScript's \s class; RE2's \s is ASCII-only.
const jsSpace = `\t\n\x{0B}\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

var (
	controlRuns = regexp.MustCompile(`[\r\n\t]+`)
	spaceRuns   = regexp.MustCompile(`[` + jsSpace + `]{2,}`)
)

// isJSSpace matches what JavaScript's trim() strips, which is wider than unicode.IsSpace.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// tidy trims and collapses runs of whitespace.
func tidy(text string) string {
	text = spaceRuns.ReplaceAllString(controlRuns.ReplaceAllString(text, " "), " ")
	return strings.TrimFunc(text, isJSSpace)
}

// shorten truncates at a sentence end if one falls close enough to the limit, else at a word
// boundary. Cutting mid-word reads as a bug, which matters most in the text explaining why a
// goal was refused. Lengths are UTF-16 code units, as in the TS.
func shorten(text string, limit int) string {
	u := utf16.Encode([]rune(text))
	if len(u) <= limit {
		return text
	}
	cut := u[:limit]
	cutStr := string(utf16.Decode(cut))
	half := float64(limit) * 0.5

	sentenceEnd := max(lastIndex16(cut, ". "), lastIndex16(cut, "! "))
	if float64(sentenceEnd) > half {
		return string(utf16.Decode(cut[:sentenceEnd+1]))
	}
	if lastSpace := lastIndex16(cut, " "); float64(lastSpace) > half {
		return string(utf16.Decode(cut[:lastSpace])) + "…"
	}
	return strings.TrimRightFunc(cutStr, isJSSpace) + "…"
}

func lastIndex16(u []uint16, sub string) int {
	s := utf16.Encode([]rune(sub))
	for i := len(u) - len(s); i >= 0; i-- {
		if string(utf16.Decode(u[i:i+len(s)])) == sub {
			return i
		}
	}
	return -1
}

// jsLen is a string's JavaScript length in UTF-16 code units.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// jsSlice is s.slice(0, n) in JavaScript.
func jsSlice(s string, n int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	return string(utf16.Decode(u[:n]))
}

var (
	proseMarks = regexp.MustCompile(`(?i)[.!?;:][` + jsSpace + `]|["'` + "`" + `]|\b(sorry|cannot|as an ai|here is|i have|note that)\b`)
	// "Subject 1", "Topic 3": filler the model produces when it runs out of real content.
	fillerName = regexp.MustCompile(`(?i)^(subject|topic|chapter|unit)[` + jsSpace + `]*\d+$`)
)

// looksLikeProse spots a name that is really a sentence, an instruction or the model talking
// to us rather than a label.
func looksLikeProse(name string) bool {
	return jsLen(name) > 60 || proseMarks.MatchString(name) || fillerName.MatchString(name)
}

// quote is JSON.stringify for a string.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// jsNumber is JavaScript's Number(v) for a JSON value; absent is undefined, which is NaN.
// ponytail: strings handle decimal and Infinity forms only, not 0x/0o/0b literals.
func jsNumber(v jsontext.Value) float64 {
	var x any
	if len(v) == 0 || json.Unmarshal(v, &x) != nil {
		return math.NaN()
	}
	switch x := x.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		return stringToNumber(x)
	case []any:
		// Number([]) is 0 and Number([x]) is Number(String(x)); anything longer is NaN.
		switch {
		case len(x) == 0:
			return 0
		case len(x) > 1:
			return math.NaN()
		}
		switch e := x[0].(type) {
		case nil:
			return 0
		case float64:
			return e
		case string:
			return stringToNumber(e)
		case []any:
			b, _ := json.Marshal(e)
			return jsNumber(b)
		}
	}
	return math.NaN()
}

var decimalLiteral = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

func stringToNumber(s string) float64 {
	s = strings.TrimFunc(s, isJSSpace)
	switch s {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if !decimalLiteral.MatchString(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return f
}

// pointerPath turns a JSON pointer ("/subjects/0/name") into zod's dotted path.
func pointerPath(p jsontext.Pointer) string {
	return strings.ReplaceAll(strings.TrimPrefix(string(p), "/"), "/", ".")
}
