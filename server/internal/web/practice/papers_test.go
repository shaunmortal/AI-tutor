package practice

// Live-database tests for paper marking: fresh question selection, papers moving the learner
// model, withdrawn questions, chosen scope, the post-test review, short answers and
// misconceptions. Same rules as practice_test.go: everything a test writes is deleted.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// fakeGrader stands in for the model's rubric grade for the rest of the test, counting calls.
func fakeGrader(t *testing.T, g rubricGrade) *int {
	calls := new(int)
	real := rubricGrader
	rubricGrader = func(context.Context, *ai.Client, string, assessment.ShortKey, string) (rubricGrade, error) {
		*calls++
		return g, nil
	}
	t.Cleanup(func() { rubricGrader = real })
	return calls
}

// goalTopic is a leaf topic of the goal's subject with at least n verified questions — the last
// one, because other packages' fixtures (running concurrently on the same database) count the
// questions on the first.
func (e *env) goalTopic(n int) string {
	var id string
	must(e.t, e.d.DB.Admin.QueryRow(e.ctx,
		`select l.id from topics l where l.parent_id = $1
		   and (select count(*) from questions q where q.topic_id = l.id and q.is_verified) >= $2
		 order by l.sort_order desc limit 1`, e.subjectID, n).Scan(&id))
	return id
}

// tempQuestion adds a verified question for the test and removes it, and anything pointing at
// it, afterwards. Its id sorts after every real one, so fixtures that take "the first verified
// question" never pick it up while it exists.
func (e *env) tempQuestion(topicID, qtype string, options []string, key string, misconceptions []string, chunkID *string) string {
	t := e.t
	t.Helper()
	var id string
	must(t, e.d.DB.Admin.QueryRow(e.ctx,
		`insert into questions (id, topic_id, question_type, body, options, correct_answer, explanation, difficulty,
		   is_verified, option_misconceptions, source_chunk_id)
		 values (('ffffffff' || substr(gen_random_uuid()::text, 9))::uuid, $1, $2, 'Test question ' || gen_random_uuid(),
		   $3, $4, 'Because the test says so.', 'medium', true, $5, $6)
		 returning id::text`, topicID, qtype, options, json.RawMessage(key), misconceptions, chunkID).Scan(&id))
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{`delete from mock_test_questions where question_id = $1`,
			`delete from assessment_questions where question_id = $1`, `delete from questions where id = $1`} {
			if _, err := e.d.DB.Admin.Exec(ctx, sql, id); err != nil {
				t.Errorf("cleanup question: %v", err)
			}
		}
	})
	return id
}

// paper makes an open mock test of exactly these questions.
func (e *env) paper(user string, questionIDs ...string) string {
	tests := store.NewMockTests(e.d.DB.Admin)
	m, err := tests.CreateTest(e.ctx, store.NewMockTest{UserID: user, Title: "Test", Source: "goal",
		TotalQuestions: len(questionIDs), DurationMinutes: 10})
	must(e.t, err)
	must(e.t, tests.AddQuestions(e.ctx, m.ID, questionIDs))
	return m.ID
}

func (e *env) count(sql string, args ...any) int {
	var n int
	must(e.t, e.d.DB.Admin.QueryRow(e.ctx, sql, args...).Scan(&n))
	return n
}

func TestMockTestsPreferUnseenQuestions(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)
	topic := e.goalTopic(1)
	// Deep enough that two 4-question papers need not share a question.
	for e.count(`select count(*) from questions where topic_id = $1 and is_verified`, topic) < 8 {
		e.tempQuestion(topic, "mcq", []string{"a", "b"}, `[0]`, nil, nil)
	}
	bank := e.count(`select count(*) from questions where topic_id = $1 and is_verified`, topic)

	seen := map[string]bool{}
	for sitting := range 2 {
		var out struct {
			Test mockTestView `json:"test"`
		}
		e.call("POST", "/mock-tests", user, map[string]any{"topicIds": []string{topic}, "questionCount": 5}).
			expect(t, 201, "").into(t, &out)
		repeats := 0
		for _, q := range out.Test.Questions {
			if q.TopicID != topic {
				t.Fatalf("question %s is outside the chosen topic", q.ID)
			}
			if seen[q.ID] {
				repeats++
			}
			seen[q.ID] = true
		}
		// The second paper repeats a question only once the bank has run out of unseen ones.
		if allowed := max(0, len(seen)+repeats-bank); sitting == 1 && (repeats > allowed || len(out.Test.Questions) < 4) {
			t.Fatalf("second paper repeated %d questions with %d unseen left", repeats, bank-len(seen)+repeats)
		}
	}
}

func TestMockTestChosenTopics(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)
	res := e.call("GET", "/mock-tests", user, nil).expect(t, 200, "")
	var list struct {
		Topics []assessment.MockTestTopic `json:"topics"`
	}
	res.into(t, &list)
	if len(list.Topics) == 0 || list.Topics[0].Name == "" {
		t.Fatalf("no goal topics to choose from: %s", res.raw)
	}

	outside := e.call("POST", "/mock-tests", user, map[string]any{"topicIds": []string{"00000000-0000-4000-8000-000000000000"}}).
		expect(t, 400, "INVALID_INPUT")
	if outside.Error.Message != "topicIds: Pick topics from your current goal." {
		t.Fatal(outside.Error.Message)
	}
	e.call("POST", "/mock-tests", user, map[string]any{"topicIds": []string{}}).expect(t, 400, "INVALID_INPUT")
	e.call("POST", "/mock-tests", user, map[string]any{"topicIds": []string{"nope"}}).expect(t, 400, "INVALID_INPUT")

	topic := e.goalTopic(2)
	var out struct {
		Test mockTestView `json:"test"`
	}
	e.call("POST", "/mock-tests", user, map[string]any{"topicIds": []string{topic, topic}}).expect(t, 201, "").into(t, &out)
	if out.Test.Source != "chosen" || len(out.Test.Coverage) != 1 {
		t.Fatalf("chosen test: %+v", out.Test)
	}
}

// A paper moves mastery, the revision schedule and the profile like practice answers, records
// misconceptions, survives a withdrawn question, and is reviewable with its sources afterwards.
func TestMockTestMarkingAndReview(t *testing.T) {
	e := setup(t)
	calls := fakeGrader(t, rubricGrade{Correct: false, Feedback: "Inertia is not a force.",
		Misconception: new("thinks inertia is a force")})
	user := e.newUser(true)
	topic := e.goalTopic(1)

	// The learner's own material, so the review can cite where a question came from.
	src, err := store.NewSources(e.d.DB.Admin).Create(e.ctx, store.NewSource{UserID: user, Kind: "pdf", Title: "Physics notes",
		StoragePath: new("test/physics.pdf")})
	must(t, err)
	page := 12
	must(t, store.NewSources(e.d.DB.Admin).InsertChunks(e.ctx, *src, []store.NewChunk{{Modality: "text", Page: &page,
		Content: "Inertia is the tendency of a body to keep its state of motion."}}))
	var chunk string
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select id from source_chunks where source_id = $1`, src.ID).Scan(&chunk))

	mcq := e.tempQuestion(topic, "mcq", []string{"Inertia", "Friction", "Gravity"}, `[0]`,
		[]string{"", "confuses friction with inertia", "thinks gravity resists motion"}, &chunk)
	short := e.tempQuestion(topic, "short", nil, `{"answers": ["inertia"], "rubric": "Names inertia."}`, nil, nil)
	withdrawn := e.tempQuestion(topic, "mcq", []string{"a", "b"}, `[1]`, nil, nil)
	skipped := e.tempQuestion(topic, "mcq", []string{"a", "b"}, `[1]`, nil, nil)
	testID := e.paper(user, mcq, short, withdrawn, skipped)

	// Before submission nothing of the key is reachable.
	open := e.call("GET", "/mock-tests/"+testID, user, nil).expect(t, 200, "")
	noAnswerKey(t, open)
	if strings.Contains(open.raw, "misconception") || strings.Contains(open.raw, "inertia is") {
		t.Fatalf("open test leaks marking: %s", open.raw)
	}

	// Pulled from the bank between sitting and submitting: must not cost the learner the paper.
	_, err = e.d.DB.Admin.Exec(e.ctx, `update questions set is_verified = false where id = $1`, withdrawn)
	must(t, err)

	res := e.call("POST", "/mock-tests/"+testID+"/submit", user, map[string]any{"secondsTaken": 300, "answers": []any{
		map[string]any{"questionId": mcq, "selectedOptions": []int{1}},
		map[string]any{"questionId": short, "text": "a force that keeps things moving"},
		map[string]any{"questionId": withdrawn, "selectedOptions": []int{1}},
	}}).expect(t, 200, "")
	var out struct {
		Test mockTestView `json:"test"`
	}
	res.into(t, &out)
	r := out.Test.Result
	if *calls != 1 || r == nil || r.TotalQuestions != 3 || r.AnsweredCount != 2 || r.CorrectCount != 0 {
		t.Fatalf("marking (grader calls %d): %s", *calls, res.raw)
	}

	status := map[string]reviewItem{}
	for _, item := range out.Test.Review {
		status[item.QuestionID] = item
	}
	m, s := status[mcq], status[short]
	if len(out.Test.Review) != 4 || m.Status != "wrong" || s.Status != "wrong" || status[withdrawn].Status != "unmarked" ||
		status[skipped].Status != "skipped" || given(status[skipped].GivenAnswer) {
		t.Fatalf("review statuses: %s", res.raw)
	}
	if string(m.CorrectAnswer) != "[0]" || m.Explanation == nil || m.Misconception == nil || *m.Misconception != "confuses friction with inertia" ||
		m.Source == nil || m.Source.Location != "p. 12" || m.Source.SourceTitle != "Physics notes" || m.Source.ChunkID != chunk {
		t.Fatalf("mcq review: %+v", m)
	}
	if s.Feedback == nil || *s.Feedback != "Inertia is not a force." || s.Misconception == nil || !strings.Contains(string(s.CorrectAnswer), "rubric") {
		t.Fatalf("short review: %+v", s)
	}

	// Misconceptions on the weak topic, with the excerpt to revisit.
	if len(r.Misconceptions) != 1 || r.Misconceptions[0].TopicID != topic || len(r.Misconceptions[0].Misconceptions) != 2 {
		t.Fatalf("misconceptions: %+v", r.Misconceptions)
	}
	cited := false
	for _, mc := range r.Misconceptions[0].Misconceptions {
		cited = cited || (mc.Label == "confuses friction with inertia" && mc.Source != nil && mc.Source.ChunkID == chunk)
	}
	if !cited {
		t.Fatalf("misconception without its source: %+v", r.Misconceptions[0])
	}

	// The learner model moved exactly as practice would have moved it.
	if n := e.count(`select count(*) from question_attempts where user_id = $1 and source = 'mock_test'`, user); n != 2 {
		t.Fatalf("%d attempts, want 2 (the withdrawn question records none)", n)
	}
	if n := e.count(`select count(*) from concept_mastery where user_id = $1 and topic_id = $2 and total_attempts = 2`, user, topic); n != 1 {
		t.Fatal("mastery not recomputed")
	}
	if n := e.count(`select count(*) from revision_schedule where user_id = $1 and topic_id = $2`, user, topic); n != 1 {
		t.Fatal("revision schedule not moved")
	}
	if n := e.count(`select total_attempts from learner_profiles where user_id = $1`, user); n != 2 {
		t.Fatalf("profile counts %d answers, want 2", n)
	}
	if n := e.count(`select count(*) from mistake_patterns where user_id = $1`, user); n != 2 {
		t.Fatalf("%d mistake patterns, want 2", n)
	}

	// Idempotent, and the dashboard sees the same misconceptions.
	e.call("POST", "/mock-tests/"+testID+"/submit", user, map[string]any{"secondsTaken": 1, "answers": []any{}}).expect(t, 200, "")
	if n := e.count(`select count(*) from question_attempts where user_id = $1`, user); n != 2 {
		t.Fatalf("resubmit wrote attempts: %d", n)
	}
	var dash struct {
		Topics []topicMisconceptions `json:"topics"`
	}
	e.call("GET", "/misconceptions", user, nil).expect(t, 200, "").into(t, &dash)
	if len(dash.Topics) != 1 || len(dash.Topics[0].Misconceptions) != 2 {
		t.Fatalf("dashboard misconceptions: %+v", dash)
	}
	// Somebody else's review is not reachable.
	e.call("GET", "/mock-tests/"+testID, e.newUser(false), nil).expect(t, 404, "TEST_NOT_FOUND")
}

func TestDiagnosticMovesLearnerModel(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)
	var started struct {
		Assessment assessmentView `json:"assessment"`
	}
	e.call("POST", "/assessments/diagnostic", user, map[string]any{}).expect(t, 201, "").into(t, &started)
	a := started.Assessment
	answers := make([]map[string]any, len(a.Questions))
	for i, q := range a.Questions {
		answers[i] = e.answerFor(q.ID, i%2 == 0)
	}
	e.call("POST", "/assessments/"+a.ID+"/submit", user, map[string]any{"answers": answers}).expect(t, 200, "")
	if n := e.count(`select total_attempts from learner_profiles where user_id = $1`, user); n != len(answers) {
		t.Fatalf("profile counts %d answers, want %d", n, len(answers))
	}
	if n := e.count(`select count(*) from revision_schedule where user_id = $1`, user); n == 0 {
		t.Fatal("diagnostic did not schedule revision")
	}
	if n := e.count(`select count(*) from assessment_questions where assessment_id = $1 and attempt_id is not null`, a.ID); n != len(answers) {
		t.Fatalf("%d answers linked, want %d", n, len(answers))
	}
}

func TestShortAnswerPractice(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)
	q := e.tempQuestion(e.goalTopic(1), "short", nil, `{"answers": ["Newton's first law"], "rubric": "Names the first law."}`, nil, nil)

	// A wording the key accepts is marked without a model, and the key comes back only now.
	var right attemptResult
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "text": "  newton's FIRST law. "}).
		expect(t, 201, "").into(t, &right)
	if !right.IsCorrect || right.Feedback != nil || !strings.Contains(string(right.CorrectAnswer), "Newton's first law") {
		t.Fatalf("exact match: %+v", right)
	}

	// Anything else needs the rubric grader; with no model configured nothing is written.
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "text": "law of inertia"}).expect(t, 503, "AI_UNAVAILABLE")
	if n := e.count(`select count(*) from question_attempts where user_id = $1`, user); n != 1 {
		t.Fatalf("%d attempts after a failed grade", n)
	}
	calls := fakeGrader(t, rubricGrade{Correct: true, Feedback: "Inertia is the first law's subject — accepted."})
	var judged attemptResult
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "text": "law of inertia"}).expect(t, 201, "").into(t, &judged)
	if *calls != 1 || !judged.IsCorrect || judged.Feedback == nil || judged.Misconception != nil || judged.Mastery.AttemptsOnTopic < 2 {
		t.Fatalf("rubric grade: %+v", judged)
	}
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "selectedOptions": []int{0}}).expect(t, 400, "INVALID_ANSWER")
}

func TestWrongOptionRecordsMisconception(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)
	q := e.tempQuestion(e.goalTopic(1), "mcq", []string{"right", "tempting"}, `[0]`, []string{"", "falls for the tempting one"}, nil)
	var res attemptResult
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "selectedOptions": []int{1}}).expect(t, 201, "").into(t, &res)
	if res.Misconception == nil || *res.Misconception != "falls for the tempting one" {
		t.Fatalf("misconception: %+v", res)
	}
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "selectedOptions": []int{1}}).expect(t, 201, "")
	if n := e.count(`select occurrences from mistake_patterns where user_id = $1`, user); n != 2 {
		t.Fatalf("occurrences %d", n)
	}
	// A right answer records nothing.
	e.call("POST", "/attempts", user, map[string]any{"questionId": q, "selectedOptions": []int{0}}).expect(t, 201, "")
	if n := e.count(`select count(*) from mistake_patterns where user_id = $1`, user); n != 1 {
		t.Fatalf("%d patterns", n)
	}
}
