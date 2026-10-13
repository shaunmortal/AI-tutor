package learning

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A course map built from one learner's material is theirs: another learner can neither point a
// goal at it nor read its topics, through the API or straight from the tables under RLS. Shared
// syllabi stay readable by everybody.
func TestCourseMapIsPrivateEverywhere(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	owner, other := e.u1, e.u2

	var mapID, subjectID string
	must(t, e.database.Admin.QueryRow(ctx,
		`insert into curricula (slug, name, is_ai_generated, is_course_map)
		 values ('test-private-map-' || gen_random_uuid(), 'Private map', true, true) returning id`).Scan(&mapID))
	t.Cleanup(func() { _, _ = e.database.Admin.Exec(ctx, `delete from curricula where id = $1`, mapID) })
	must(t, e.database.Admin.QueryRow(ctx,
		`insert into topics (curriculum_id, name) values ($1, 'Secret subject') returning id`, mapID).Scan(&subjectID))
	_, err := e.database.Admin.Exec(ctx,
		`insert into topics (curriculum_id, parent_id, name) values ($1, $2, 'Secret topic')`, mapID, subjectID)
	must(t, err)
	_, err = e.database.Admin.Exec(ctx,
		`insert into course_sources (user_id, curriculum_id, kind, title, storage_path, status)
		 values ($1, $2, 'pdf', 'Owner notes', 'test/owner.pdf', 'ready')`, owner, mapID)
	must(t, err)

	topicsVisibleTo := func(user, curriculumID string) int {
		var n int
		must(t, e.database.AsUser(ctx, user, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select count(*) from topics where curriculum_id = $1`, curriculumID).Scan(&n)
		}))
		return n
	}

	// RLS: only the owner sees the map; everybody sees the shared syllabus.
	check(t, topicsVisibleTo(owner, mapID) == 2, "owner should see the map's topics")
	check(t, topicsVisibleTo(other, mapID) == 0, "another learner must not see the map's topics")
	check(t, topicsVisibleTo(other, e.curriculumID) > 0, "the shared syllabus must stay readable")

	// API: the goal route can't be used to get at someone else's map.
	body := `{"title":"Exam","examDate":"` + inDays(30) + `","dailyMinutes":60,"curriculumId":"` + mapID +
		`","subjectIds":["` + subjectID + `"]}`
	e.expect(e.call(other, "POST", "/goals", body), http.StatusNotFound, "CURRICULUM_NOT_FOUND", "")
	subjects := e.call(other, "GET", "/goals/subjects?curriculumId="+mapID, "")
	e.expect(subjects, http.StatusOK, "", "")
	list, _ := subjects.Data["subjects"].([]any)
	check(t, len(list) == 0, "another learner listed %d subjects of a private map", len(list))

	// The owner can still build a goal on their own map.
	e.expect(e.call(owner, "POST", "/goals", body), http.StatusCreated, "", "")
}
