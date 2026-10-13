package curriculum

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The safety net on AI-written syllabi. Every case is something a model plausibly returns: a
// shape that is fine but a syllabus that is not.

type obj = map[string]any

func subj(name string, weight any, topics ...string) obj {
	return obj{"name": name, "weight": weight, "topics": topics}
}

// replyJSON is a good reply with overrides applied, encoded as the model would send it.
func replyJSON(t *testing.T, overrides obj) []byte {
	t.Helper()
	r := obj{
		"isStudyGoal": true,
		"reason":      "",
		"name":        "Class 10 (CBSE)",
		"slug":        "class-10-cbse",
		"description": "The CBSE class 10 syllabus.",
		"subjects": []obj{
			subj("Mathematics", 1.5, "Real Numbers", "Quadratic Equations", "Trigonometry"),
			subj("Science", 1.5, "Chemical Reactions", "Light", "Life Processes"),
		},
	}
	for k, v := range overrides {
		r[k] = v
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var dbSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,79}$`)

func TestToSlug(t *testing.T) {
	// These must all hit the same stored syllabus rather than generating copies of it.
	for in, want := range map[string]string{
		"class 10":       "class-10",
		"Class 10":       "class-10",
		"  CLASS   10  ": "class-10",
		"12th Physics!":  "12th-physics",
		"Mathématiques":  "mathematiques",
		"!!!":            "",
		"   ":            "",
	} {
		if got := ToSlug(in); got != want {
			t.Errorf("ToSlug(%q) = %q, want %q", in, got, want)
		}
	}
	// Never a slug the database would reject.
	for _, in := range []string{"---", "!!!", "GATE — CSE", strings.Repeat("a", 200)} {
		if s := ToSlug(in); len(s) >= 2 && !dbSlug.MatchString(s) {
			t.Errorf("ToSlug(%q) = %q violates the column constraint", in, s)
		}
	}
}

func refused(t *testing.T, o Outcome) string {
	t.Helper()
	if o.OK {
		t.Fatalf("accepted, want refusal: %+v", o.Curriculum)
	}
	return o.LearnerMessage
}

func accepted(t *testing.T, o Outcome) Resolved {
	t.Helper()
	if !o.OK {
		t.Fatalf("refused: %s (%s)", o.LearnerMessage, o.LogDetail)
	}
	return o.Curriculum
}

func notGoal(reason string) obj {
	return obj{"isStudyGoal": false, "reason": reason, "name": "", "slug": "", "subjects": []obj{}}
}

func TestValidateReplyRejectsNonGoals(t *testing.T) {
	msg := refused(t, ValidateReply(replyJSON(t, notGoal("A dog is an animal, not a subject.")), "dog"))
	if !regexp.MustCompile(`(?i)not look like something to study`).MatchString(msg) || !strings.Contains(msg, "A dog is an animal") {
		t.Errorf("with reason: %q", msg)
	}

	// No reason: still suggests what a valid goal looks like.
	msg = refused(t, ValidateReply(replyJSON(t, notGoal("")), "apple"))
	if !strings.Contains(msg, "apple") || !regexp.MustCompile(`(?i)class 10|GATE|python`).MatchString(msg) {
		t.Errorf("without reason: %q", msg)
	}
}

// Seen live on "apple": the model refused at length, the reason overflowed a schema cap, and
// the learner got "something went wrong" from a system that had worked exactly as designed.
func TestValidateReplyKeepsALongRefusal(t *testing.T) {
	long := "An apple is a fruit rather than a field of study. " +
		strings.Repeat("While there are subjects that touch on apples — botany covers the biology of fruiting plants, ", 8) +
		"none of these is what the word on its own names."
	if jsLen(long) <= 400 {
		t.Fatalf("reason only %d long", jsLen(long))
	}
	msg := refused(t, ValidateReply(replyJSON(t, notGoal(long)), "apple"))
	if !regexp.MustCompile(`(?i)not look like something to study`).MatchString(msg) ||
		regexp.MustCompile(`(?i)something went wrong`).MatchString(msg) ||
		jsLen(msg) >= 340 ||
		regexp.MustCompile(`\w…$`).MatchString(msg) {
		t.Errorf("got %q", msg)
	}
}

func TestValidateReplyTrimsAPaddedDescription(t *testing.T) {
	c := accepted(t, ValidateReply(replyJSON(t, obj{"description": strings.Repeat("The CBSE class 10 syllabus. ", 200)}), "class 10"))
	if jsLen(c.Description) > 2000 || len(c.Subjects) != 2 {
		t.Errorf("description %d long, %d subjects", jsLen(c.Description), len(c.Subjects))
	}
}

func TestValidateReplyRejectsTheWrongShape(t *testing.T) {
	for _, raw := range []string{`{"isStudyGoal": "yes"}`, `"sorry, I cannot"`, `null`} {
		if o := ValidateReply([]byte(raw), "x"); o.OK {
			t.Errorf("%s accepted", raw)
		}
	}
}

func TestValidateReplyAcceptsAGoodSyllabus(t *testing.T) {
	c := accepted(t, ValidateReply(replyJSON(t, nil), "class 10"))
	if c.Name != "Class 10 (CBSE)" || c.Slug != "class-10-cbse" || len(c.Subjects) != 2 {
		t.Errorf("got %+v", c)
	}

	// The slug is regenerated rather than trusted.
	if c := accepted(t, ValidateReply(replyJSON(t, obj{"slug": "Class 10 !! CBSE"}), "class 10")); !dbSlug.MatchString(c.Slug) {
		t.Errorf("slug %q", c.Slug)
	}
	if c := accepted(t, ValidateReply(replyJSON(t, obj{"slug": "", "name": "Class 10"}), "class 10")); c.Slug != "class-10" {
		t.Errorf("fallback slug %q", c.Slug)
	}
	if c := accepted(t, ValidateReply(replyJSON(t, obj{"name": "  Class   10  \n"}), "class 10")); c.Name != "Class 10" {
		t.Errorf("name %q", c.Name)
	}
}

func subjectNames(c Resolved) []string {
	var out []string
	for _, s := range c.Subjects {
		out = append(out, s.Name)
	}
	return out
}

func TestValidateReplyCleansUp(t *testing.T) {
	cases := []struct {
		name     string
		subjects []obj
		want     []string
	}{
		// The topics table would reject a duplicate subject anyway.
		{"duplicate subject", []obj{subj("Maths", 1, "A one", "B two"), subj("maths", 1, "C three", "D four")}, []string{"Maths"}},
		{"one-topic subject is a topic one level too high", []obj{subj("Real Subject", 1, "A one", "B two"), subj("Stray", 1, "Only one")}, []string{"Real Subject"}},
		{"filler name", []obj{subj("Real Subject", 1, "A one", "B two"), subj("Subject 2", 1, "C three", "D four")}, []string{"Real Subject"}},
		{"the model talking to us", []obj{subj("Real Subject", 1, "A one", "B two"), subj("Sorry, I cannot build a syllabus for that request.", 1, "C three", "D four")}, []string{"Real Subject"}},
	}
	for _, c := range cases {
		got := subjectNames(accepted(t, ValidateReply(replyJSON(t, obj{"subjects": c.subjects}), "class 10")))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	c := accepted(t, ValidateReply(replyJSON(t, obj{"subjects": []obj{subj("Maths", 1, "Trigonometry", "trigonometry ", "Algebra")}}), "class 10"))
	if n := len(c.Subjects[0].Topics); n != 2 {
		t.Errorf("repeated topic: %d topics, want 2", n)
	}
}

func TestValidateReplyClampsWeights(t *testing.T) {
	// A loose number is the model being sloppy about one field, not a wrong syllabus. The TS test
	// passes NaN, which JSON carries as null; missing and junk strings fall back to 1 too.
	missing := obj{"name": "Absent", "topics": []string{"G seven", "H eight"}}
	c := accepted(t, ValidateReply(replyJSON(t, obj{"subjects": []obj{
		subj("Huge", 900, "A one", "B two"),
		subj("Tiny", -5, "C three", "D four"),
		subj("Missing", nil, "E five", "F six"),
		missing,
		subj("Junk", "abc", "I nine", "J ten"),
		subj("Stringy", " 2.5 ", "K eleven", "L twelve"),
	}}), "class 10"))
	var got []float64
	for _, s := range c.Subjects {
		got = append(got, s.Weight)
	}
	if want := []float64{3, 0.5, 1, 1, 1, 2.5}; !slices.Equal(got, want) {
		t.Errorf("weights %v, want %v", got, want)
	}
}

func TestValidateReplyCapsARunawaySyllabus(t *testing.T) {
	var subjects []obj
	for s := range 20 {
		topics := make([]string, 30)
		for i := range topics {
			topics[i] = fmt.Sprintf("Topic %d-%d", s, i)
		}
		subjects = append(subjects, subj("Subject named "+string(rune('A'+s)), 1, topics...))
	}
	c := accepted(t, ValidateReply(replyJSON(t, obj{"subjects": subjects}), "class 10"))
	total := 0
	for _, s := range c.Subjects {
		total += len(s.Topics)
		if len(s.Topics) > MaxTopicsPerSubject {
			t.Errorf("%s has %d topics", s.Name, len(s.Topics))
		}
	}
	if len(c.Subjects) > 12 || total > 90 {
		t.Errorf("%d subjects, %d topics", len(c.Subjects), total)
	}
}

func TestValidateReplyRejectsWhenNoSubjectSurvives(t *testing.T) {
	// Both dropped by the business rules rather than failing the whole parse.
	msg := refused(t, ValidateReply(replyJSON(t, obj{"subjects": []obj{
		subj("Subject 1", 1, "A one", "B two"),
		subj("x", 1, "C three"),
	}}), "class 10"))
	if !regexp.MustCompile(`(?i)more specific`).MatchString(msg) {
		t.Errorf("got %q", msg)
	}
}

func TestSchemaErrorsNameTheField(t *testing.T) {
	cases := map[string]string{
		`{"isStudyGoal": "yes"}`: "isStudyGoal",
		`{"isStudyGoal": true, "reason": "", "name": "n", "slug": "s", "description": "d", "subjects": [{"name": 5, "topics": []}]}`: "subjects.0.name",
		`{"isStudyGoal": true, "reason": "", "name": "n", "slug": "s", "description": "d", "subjects": [{"name": "x"}]}`:             "subjects.0.topics",
		`{"isStudyGoal": true, "reason": "", "name": "n", "slug": "s", "description": "d"}`:                                          "subjects",
	}
	for raw, path := range cases {
		o := ValidateReply([]byte(raw), "x")
		if o.OK || !strings.HasPrefix(o.LogDetail, "reply failed the schema: "+path+" ") ||
			!strings.Contains(o.LearnerMessage, "Something went wrong") {
			t.Errorf("%s: %+v", raw, o)
		}
	}
}
