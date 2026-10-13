package content

// The tutor's quick checks, revision flashcards and the course map built when ingestion
// finishes, against the live database with a fake model (ai.NewFake): never the real API.

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// readyExcerpt gives user a ready source with one excerpt tagged with a shipped topic, and
// returns the topic and the excerpt.
func (e *env) readyExcerpt(user, content string) (topicID, chunkID string) {
	e.t.Helper()
	must(e.t, e.s.d.DB.Admin.QueryRow(e.ctx, `select t.id::text from topics t join curricula c on c.id = t.curriculum_id
		where c.slug = $1 and t.parent_id is not null order by t.sort_order, t.name limit 1`, store.DefaultCurriculumSlug).Scan(&topicID))
	path := user + "/notes.pdf"
	src, err := e.s.sources().Create(e.ctx, store.NewSource{UserID: user, Kind: "pdf", Title: "Notes", StoragePath: &path})
	must(e.t, err)
	must(e.t, e.s.sources().InsertChunks(e.ctx, *src, []store.NewChunk{{Modality: "text", Page: ptr(3), Content: content}}))
	must(e.t, e.s.sources().SetStatus(e.ctx, src.ID, "ready", nil))
	chunkID = e.firstChunk(src.ID)
	must(e.t, e.s.sources().TagChunks(e.ctx, []store.ChunkAssignment{{ChunkID: chunkID, TopicID: topicID}}))
	return topicID, chunkID
}

// dropTutorQuestions removes the check questions the test's learners were given. They outlive
// their creator (created_by is set null), so they go before the users do.
func (e *env) dropTutorQuestions() {
	e.t.Cleanup(func() {
		_, err := e.s.d.DB.Admin.Exec(e.ctx, `delete from questions where created_by = any($1::uuid[])`, []string{e.u1, e.u2})
		must(e.t, err)
	})
}

func TestTutorQuickCheckMovesTheLearnerModel(t *testing.T) {
	e := setup(t)
	e.dropTutorQuestions()
	topicID, chunkID := e.readyExcerpt(e.u1, "A primary key identifies each row of a table uniquely.")
	e.s.d.AI = ai.NewFake(func(r ai.Request) (string, error) {
		return `{"grounding":"source","answer":"It identifies each row uniquely [1].","outsideKnowledge":"",
			"check":{"question":"What does a primary key do?","options":["Identifies each row","Sorts the table","Encrypts rows","Joins tables"],
			"correctIndex":0,"explanation":"It identifies each row.","sourceExcerpt":1}}`, nil
	})

	var reply ai.TutorReply
	r := e.json(e.u1, "POST", "/ai/tutor", map[string]string{"question": "What does a primary key do?"})
	r.into(t, &reply)
	if r.status != 201 || reply.Grounding == nil || *reply.Grounding != ai.GroundingSource || reply.Check == nil ||
		reply.TopicID == nil || *reply.TopicID != topicID || reply.Check.Citation.ChunkID != chunkID {
		t.Fatalf("ask: %d %s", r.status, r.Data)
	}
	if strings.Contains(string(r.Data), "correctIndex") || strings.Contains(string(r.Data), "It identifies each row.\"") {
		t.Fatalf("the check's answer leaked before it was answered: %s", r.Data)
	}
	// The conversation now records its topic, inferred from the cited excerpt.
	if got, _ := e.col(`select coalesce(topic_id::text, '') from ai_conversations where id = $1`, reply.ConversationID); len(got) != 1 || got[0] != topicID {
		t.Fatalf("conversation topic: %v", got)
	}
	// The check is never in the practice bank.
	if got, _ := e.col(`select is_verified::text from questions where id = $1`, reply.Check.QuestionID); len(got) != 1 || got[0] != "false" {
		t.Fatalf("check question: %v", got)
	}

	path := "/ai/checks/" + reply.Check.QuestionID + "/answer"
	expectError(t, e.json(e.u1, "POST", path, map[string]any{}), 400, "INVALID_INPUT", "")
	expectError(t, e.json(e.u2, "POST", path, map[string]int{"selectedOption": 0}), 404, "QUESTION_NOT_FOUND", "")

	var graded struct {
		IsCorrect     bool
		CorrectAnswer []int
		SourceChunkID *string
		Mastery       struct {
			TopicID string
			After   float64
		}
	}
	r = e.json(e.u1, "POST", path, map[string]int{"selectedOption": 0})
	r.into(t, &graded)
	if r.status != 201 || !graded.IsCorrect || len(graded.CorrectAnswer) != 1 || graded.SourceChunkID == nil ||
		*graded.SourceChunkID != chunkID || graded.Mastery.TopicID != topicID || graded.Mastery.After <= 0 {
		t.Fatalf("answer: %d %s", r.status, r.Data)
	}
	// Recorded like practice: a 'tutor' attempt, mastery and the revision schedule.
	got, err := e.col(`select a.source || '/' || a.is_correct || '/' ||
		(select count(*) from concept_mastery m where m.user_id = a.user_id and m.topic_id = a.topic_id) || '/' ||
		(select count(*) from revision_schedule s where s.user_id = a.user_id and s.topic_id = a.topic_id)
		from question_attempts a where a.user_id = $1`, e.u1)
	must(t, err)
	if len(got) != 1 || got[0] != "tutor/true/1/1" {
		t.Fatalf("recorded: %v", got)
	}
}

func TestFlashcardsAreWrittenOnceAndMoveTheSchedule(t *testing.T) {
	e := setup(t)
	topicID, chunkID := e.readyExcerpt(e.u1, "Normalisation removes redundancy from relational tables.")
	var calls atomic.Int32
	e.s.d.AI = ai.NewFake(func(r ai.Request) (string, error) {
		calls.Add(1)
		if r.System != ai.FlashcardSystem || !strings.Contains(r.Prompt, "Normalisation removes redundancy") {
			t.Errorf("unexpected request: %s", r.System)
		}
		return `{"cards":[{"front":"What does normalisation remove?","back":"Redundancy.","sourceExcerpt":1},
			{"front":"Made up","back":"From nowhere","sourceExcerpt":9}]}`, nil
	})

	// Nothing due or weak yet: an empty deck, and no model call.
	var deck struct {
		Topics []struct {
			TopicID string
			Reason  string
			Cards   []struct {
				ID, Front, Back string
				SourceChunkID   *string
				Citations       []ai.Citation
			}
		}
	}
	e.do(e.u1, "GET", "/revision/flashcards", "", nil).into(t, &deck)
	if len(deck.Topics) != 0 || calls.Load() != 0 {
		t.Fatalf("empty deck: %+v, %d calls", deck, calls.Load())
	}

	today := dates.TodayIn(dates.DefaultTimezone, time.Now())
	twoDaysAgo := dates.AddDays(today, -2)
	must(t, store.NewRevisions(e.s.d.DB.Admin).Save(e.ctx, e.u1, topicID, learner.ScheduleUpdate{
		DueOn: dates.AddDays(today, -1), IntervalDays: 3, EaseFactor: 2.5, ReviewCount: 1, LastReviewedOn: twoDaysAgo, LastReviewAccuracy: 90}))

	for range 2 {
		r := e.do(e.u1, "GET", "/revision/flashcards", "", nil)
		r.into(t, &deck)
		if r.status != 200 || len(deck.Topics) != 1 || deck.Topics[0].Reason != "due" || len(deck.Topics[0].Cards) != 1 {
			t.Fatalf("deck: %d %s", r.status, r.Data)
		}
		c := deck.Topics[0].Cards[0]
		if c.SourceChunkID == nil || *c.SourceChunkID != chunkID || len(c.Citations) != 1 ||
			c.Citations[0].ChunkID != chunkID || c.Citations[0].Location != "p. 3" || c.Citations[0].SourceTitle != "Notes" {
			t.Fatalf("card: %+v", c)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("deck written %d times", calls.Load())
	}

	card := "/revision/flashcards/" + deck.Topics[0].Cards[0].ID + "/review"
	expectError(t, e.json(e.u2, "POST", card, map[string]bool{"known": true}), 404, "FLASHCARD_NOT_FOUND", "")
	expectError(t, e.json(e.u1, "POST", card, map[string]any{}), 400, "INVALID_INPUT", "")

	var moved struct {
		Moved    bool
		Schedule *learner.ScheduleUpdate
	}
	e.json(e.u1, "POST", card, map[string]bool{"known": false}).into(t, &moved)
	if !moved.Moved || moved.Schedule.IntervalDays != 1 || moved.Schedule.Lapses != 1 || moved.Schedule.DueOn != dates.AddDays(today, 1) {
		t.Fatalf("again: %+v", moved.Schedule)
	}
	// A second mark the same day does not improve it.
	e.json(e.u1, "POST", card, map[string]bool{"known": true}).into(t, &moved)
	if moved.Moved {
		t.Fatal("known after again moved the schedule the same day")
	}
	if got, _ := e.col(`select due_on::text from revision_schedule where user_id = $1 and topic_id = $2`, e.u1, topicID); len(got) != 1 || got[0] != dates.AddDays(today, 1) {
		t.Fatalf("stored schedule: %v", got)
	}

	// No model: a learner with due topics but no cards hears why.
	e.s.d.AI = nil
	_, err := e.s.d.DB.Admin.Exec(e.ctx, `delete from flashcards where user_id = $1`, e.u1)
	must(t, err)
	_, err = e.s.d.DB.Admin.Exec(e.ctx, `update revision_schedule set due_on = due_on - 2 where user_id = $1`, e.u1)
	must(t, err)
	expectError(t, e.do(e.u1, "GET", "/revision/flashcards", "", nil), 503, "AI_UNAVAILABLE", "")
}

func TestCourseMapIsBuiltWhenIngestionFinishes(t *testing.T) {
	e := setup(t)
	var curricula []string
	t.Cleanup(func() {
		e.s.d.Redis.Del(e.ctx, organizePendingKey(e.u1), organizeFailedKey(e.u1))
		curricula, _ = e.col(`select distinct curriculum_id::text from course_sources where user_id = $1 and curriculum_id is not null`, e.u1)
		_, err := e.s.d.DB.Admin.Exec(e.ctx, `delete from curricula where id = any($1::uuid[])`, curricula)
		must(t, err)
	})
	e.s.d.AI = ai.NewFake(func(r ai.Request) (string, error) {
		if r.System != ai.CourseMapSystem {
			t.Errorf("unexpected request: %s", r.System)
		}
		return `{"subjects":[{"name":"Heaps","weight":2,"topics":[
			{"name":"Heap basics","keyConcepts":["heap"],"prerequisites":[],"excerptIds":[0]},
			{"name":"Heap sort","keyConcepts":["sorting"],"prerequisites":["Heap basics"],"excerptIds":[]}]}]}`, nil
	})

	// Still processing (no worker yet): the map waits for the material.
	video := e.add(e.u1, "/sources/video/captions?url=https://youtu.be/dQw4w9WgXcQ&title=Lecture", "text/vtt", []byte(vtt))
	r := e.json(e.u1, "POST", "/sources/organize", map[string]any{"courseName": "Data structures", "whenReady": true})
	if r.status != 202 || !strings.Contains(string(r.Data), `"waiting"`) {
		t.Fatalf("organize: %d %s", r.status, r.Data)
	}
	var list struct {
		Organize struct{ Status string }
	}
	e.do(e.u1, "GET", "/sources", "", nil).into(t, &list)
	if list.Organize.Status != "waiting" {
		t.Fatalf("status: %+v", list)
	}

	e.runWorker()
	e.waitFor(e.u1, video)
	deadline := time.Now().Add(20 * time.Second)
	for {
		var out struct {
			Sources  []sourceView
			Organize struct{ Status string }
		}
		e.do(e.u1, "GET", "/sources", "", nil).into(t, &out)
		if out.Organize.Status == "idle" && out.Sources[0].CurriculumID != nil {
			var m courseMapView
			e.do(e.u1, "GET", "/sources/map/"+*out.Sources[0].CurriculumID, "", nil).into(t, &m)
			raw, _ := json.Marshal(m)
			if len(m.Subjects) != 1 || len(m.Subjects[0].Topics) != 2 || m.Subjects[0].Topics[0].ExcerptCount != 1 ||
				len(m.Subjects[0].Topics[1].Prerequisites) != 1 {
				t.Fatalf("course map: %s", raw)
			}
			// What the planner and the recommendation engine are handed.
			basics, sort := m.Subjects[0].Topics[0].ID, m.Subjects[0].Topics[1].ID
			pre, err := store.NewPlans(e.s.d.DB.Admin).FindPrerequisites(e.ctx, []string{basics, sort})
			must(t, err)
			if len(pre) != 1 || len(pre[sort]) != 1 || pre[sort][0] != basics {
				t.Fatalf("prerequisites: %v", pre)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("course map never built: %+v", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
