package assessment

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"testing"
)

// The diagnostic's job is to find out where a learner is weak, so the property that matters is
// breadth. Most of these tests are about coverage rather than scoring.

// subject builds count topics inside one subject.
func subject(id, name string, weight float64, count, available int) []DiagnosticTopic {
	out := make([]DiagnosticTopic, count)
	for i := range out {
		out[i] = DiagnosticTopic{
			TopicID: fmt.Sprintf("%s-t%d", id, i), Name: fmt.Sprintf("%s topic %d", name, i),
			SubjectID: id, SubjectName: name, SubjectWeight: weight, AvailableQuestions: available,
		}
	}
	return out
}

func syllabus() []DiagnosticTopic {
	return slices.Concat(
		subject("s1", "Algorithms", 1.5, 8, 5),
		subject("s2", "Digital Logic", 1.0, 6, 5),
		subject("s3", "Databases", 1.2, 5, 5),
		subject("s4", "Aptitude", 0.8, 4, 5),
	)
}

func distinct[T comparable](xs []T) int {
	seen := map[T]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return len(seen)
}

func pickField(picks []DiagnosticPick, f func(DiagnosticPick) string) []string {
	out := make([]string, len(picks))
	for i, p := range picks {
		out[i] = f(p)
	}
	return out
}

func TestPlanDiagnosticNeverRepeatsATopic(t *testing.T) {
	ids := pickField(PlanDiagnostic(syllabus(), 15).Picks, func(p DiagnosticPick) string { return p.TopicID })
	if distinct(ids) != len(ids) {
		t.Fatalf("repeated topic: %v", ids)
	}
}

func TestPlanDiagnosticCoversEverySubject(t *testing.T) {
	subjects := pickField(PlanDiagnostic(syllabus(), 15).Picks, func(p DiagnosticPick) string { return p.SubjectID })
	if distinct(subjects) != 4 {
		t.Fatalf("covered %d subjects, want 4", distinct(subjects))
	}
}

func TestPlanDiagnosticCapsOneSubject(t *testing.T) {
	lopsided := slices.Concat(
		subject("deep", "Deep", 1.5, 20, 5),
		subject("thin1", "Thin one", 1.0, 1, 5),
		subject("thin2", "Thin two", 1.0, 1, 5),
	)
	per := map[string]int{}
	for _, p := range PlanDiagnostic(lopsided, 15).Picks {
		per[p.SubjectID]++
	}
	if per["deep"] > DiagnosticMaxPerSubject || per["thin1"] != 1 || per["thin2"] != 1 {
		t.Fatalf("per subject: %v", per)
	}
}

func TestPlanDiagnosticMeasuresHeaviestFirst(t *testing.T) {
	many := slices.Concat(
		subject("heavy", "Heavy", 2.0, 3, 5),
		subject("mid", "Middle", 1.2, 3, 5),
		subject("light", "Light", 0.4, 3, 5),
	)
	got := pickField(PlanDiagnostic(many, 6).Picks[:2], func(p DiagnosticPick) string { return p.SubjectName })
	if !slices.Equal(got, []string{"Heavy", "Middle"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPlanDiagnosticSpreadsBeforeDoublingUp(t *testing.T) {
	firstFour := pickField(PlanDiagnostic(syllabus(), 15).Picks[:4], func(p DiagnosticPick) string { return p.SubjectID })
	if distinct(firstFour) != 4 {
		t.Fatalf("first four: %v", firstFour)
	}
}

func TestPlanDiagnosticNumbersFromOne(t *testing.T) {
	for i, p := range PlanDiagnostic(syllabus(), 12).Picks {
		if p.Position != i+1 {
			t.Fatalf("pick %d has position %d", i, p.Position)
		}
	}
}

func TestPlanDiagnosticNamesUncoveredSubjects(t *testing.T) {
	plan := PlanDiagnostic(slices.Concat(
		subject("s1", "Algorithms", 1.5, 4, 5),
		subject("empty", "Compiler Design", 1.0, 3, 0),
	), 12)
	for _, p := range plan.Picks {
		if p.SubjectID != "s1" {
			t.Fatalf("picked from %s", p.SubjectID)
		}
	}
	if len(plan.Uncovered) != 1 || plan.Uncovered[0].SubjectName != "Compiler Design" ||
		!regexp.MustCompile(`(?i)no questions`).MatchString(plan.Uncovered[0].Reason) {
		t.Fatalf("uncovered: %+v", plan.Uncovered)
	}
}

func TestPlanDiagnosticLengths(t *testing.T) {
	// Three topics, one question each: a shorter paper rather than a padded one.
	if n := len(PlanDiagnostic(subject("s1", "Only", 1.0, 3, 5), 15).Picks); n != 3 {
		t.Errorf("thin bank: %d picks, want 3", n)
	}
	if n := len(PlanDiagnostic(syllabus(), 500).Picks); n > DiagnosticMaxQuestions {
		t.Errorf("silly length: %d picks", n)
	}
	if n := len(PlanDiagnostic(nil, 15).Picks); n != 0 {
		t.Errorf("no topics: %d picks", n)
	}
	if n := len(PlanDiagnostic(subject("s1", "Empty", 1, 3, 0), 15).Picks); n != 0 {
		t.Errorf("empty bank: %d picks", n)
	}
}

func TestPlanDiagnosticReportsCoverage(t *testing.T) {
	plan := PlanDiagnostic(syllabus(), 12)
	total := 0
	for _, c := range plan.Coverage {
		total += c.QuestionCount
		if c.SubjectName == "" {
			t.Errorf("unnamed coverage entry %+v", c)
		}
	}
	if total != len(plan.Picks) {
		t.Fatalf("coverage sums to %d, picks %d", total, len(plan.Picks))
	}
}

func ans(subjectID, subjectName string, isCorrect bool, answered ...bool) DiagnosticAnswer {
	return DiagnosticAnswer{TopicID: subjectID + "-t", SubjectID: subjectID, SubjectName: subjectName,
		IsCorrect: isCorrect, Answered: len(answered) == 0 || answered[0]}
}

func TestSummariseDiagnosticBands(t *testing.T) {
	res := SummariseDiagnostic([]DiagnosticAnswer{
		ans("s1", "Strong", true), ans("s1", "Strong", true), ans("s1", "Strong", true), ans("s1", "Strong", true),
		ans("s2", "Weak", false), ans("s2", "Weak", false), ans("s2", "Weak", false), ans("s2", "Weak", true),
	})
	levels := map[string]KnowledgeLevel{}
	for _, s := range res.Subjects {
		levels[s.SubjectName] = s.Level
	}
	if levels["Strong"] != LevelStrong || levels["Weak"] != LevelWeak {
		t.Fatalf("levels: %v", levels)
	}
}

func TestSummariseDiagnosticWeakestFirst(t *testing.T) {
	res := SummariseDiagnostic([]DiagnosticAnswer{
		ans("s1", "Good", true), ans("s1", "Good", true),
		ans("s2", "Bad", false), ans("s2", "Bad", false),
		ans("s3", "Middling", true), ans("s3", "Middling", false),
	})
	var names []string
	for _, s := range res.Subjects {
		names = append(names, s.SubjectName)
	}
	if !slices.Equal(names, []string{"Bad", "Middling", "Good"}) {
		t.Fatalf("order: %v", names)
	}
}

func TestSummariseDiagnosticVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		answers []DiagnosticAnswer
		want    []string
	}{
		{"names the weak subjects, tied to the plan", []DiagnosticAnswer{
			ans("s1", "Digital Logic", false), ans("s1", "Digital Logic", false),
			ans("s2", "Algorithms", true), ans("s2", "Algorithms", true),
		}, []string{`Digital Logic`, `(?i)plan`}},
		{"half-finished paper is not a measurement", []DiagnosticAnswer{
			ans("s1", "S", false, false), ans("s1", "S", false, false),
		}, []string{`(?i)nothing was answered`}},
		{"everything solid", []DiagnosticAnswer{
			ans("s1", "A", true), ans("s1", "A", true), ans("s2", "B", true), ans("s2", "B", true),
		}, []string{`(?i)already know|keep it that way`}},
	}
	for _, c := range cases {
		v := SummariseDiagnostic(c.answers).Verdict
		for _, w := range c.want {
			if !regexp.MustCompile(w).MatchString(v) {
				t.Errorf("%s: verdict %q does not match %s", c.name, v, w)
			}
		}
	}

	res := SummariseDiagnostic([]DiagnosticAnswer{
		ans("s1", "A", true), ans("s1", "A", true), ans("s2", "B", true), ans("s2", "B", true),
	})
	for _, s := range res.Subjects {
		if s.Level != LevelStrong {
			t.Errorf("%s level %s, want strong", s.SubjectName, s.Level)
		}
	}
}

func TestSummariseDiagnosticSkippedCountsAsWrong(t *testing.T) {
	// If blank raised the score, the winning strategy would be answering only what you are sure of.
	res := SummariseDiagnostic([]DiagnosticAnswer{
		ans("s1", "S", true), ans("s1", "S", false, false), ans("s1", "S", false, false),
	})
	if math.Abs(res.AccuracyPercent-33.3) >= 0.05 || res.AnsweredCount != 1 ||
		!regexp.MustCompile(`(?i)blank`).MatchString(res.Verdict) {
		t.Fatalf("got %+v", res)
	}
}

func TestSummariseDiagnosticEmpty(t *testing.T) {
	res := SummariseDiagnostic(nil)
	if res.TotalQuestions != 0 || res.Subjects == nil || len(res.Subjects) != 0 || res.AccuracyPercent != 0 {
		t.Fatalf("got %+v", res)
	}
}
