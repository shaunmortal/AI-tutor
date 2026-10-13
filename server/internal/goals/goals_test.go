package goals

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

func TestDaysUntil(t *testing.T) {
	now := time.Date(2026, 10, 13, 23, 30, 0, 0, time.FixedZone("IST", 5*3600+1800)) // 18:00 UTC
	cases := []struct {
		date string
		days int
		ok   bool
	}{{"2026-10-13", 0, true}, {"2026-10-14", 1, true}, {"2026-10-12", -1, true}, {"2026-02-30", -225, true}, {"2026-13-01", 0, false}, {"soon", 0, false}}
	for _, c := range cases {
		days, ok := DaysUntil(c.date, now)
		if days != c.days || ok != c.ok {
			t.Errorf("DaysUntil(%q) = %d, %v; want %d, %v", c.date, days, ok, c.days, c.ok)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	s := New(store.NewGoals(nil)) // every case is refused before the database is touched
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }
	cases := []struct {
		date, want string
		subjects   []string
	}{
		{"not-a-date", "The exam date is not a real date.", []string{"a"}},
		{day(0), "The exam date must be in the future — there is nothing to plan otherwise.", []string{"a"}},
		{day(731), "Pick an exam date within the next two years.", []string{"a"}},
		{day(10), "Choose at least one subject to study.", nil},
	}
	for _, c := range cases {
		_, err := s.Create(context.Background(), CreateInput{Title: "x", ExamDate: c.date, DailyMinutes: 60, SubjectIDs: c.subjects})
		var he *httpx.Error
		if !errors.As(err, &he) || he.Status != 400 || he.Code != "INVALID_INPUT" || he.Message != c.want {
			t.Errorf("%s: got %v", c.date, err)
		}
	}
}

func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	raw, _ := os.ReadFile("../../.env")
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "DATABASE_URL="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func TestCreateAndGetActive(t *testing.T) {
	url := databaseURL()
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx); _ = conn.Close(ctx) }) // always rolled back

	var userID, curriculumID, subjectID string
	if err := tx.QueryRow(ctx, `with id as (select gen_random_uuid() as id)
		insert into auth.users (id, email) select id, id || '@adigam.test' from id returning id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `select c.id, t.id from curricula c join topics t on t.curriculum_id = c.id and t.parent_id is null
		where c.slug = $1 order by t.sort_order limit 1`, store.DefaultCurriculumSlug).Scan(&curriculumID, &subjectID); err != nil {
		t.Fatal(err)
	}

	s := New(store.NewGoals(tx))
	none, err := s.GetActive(ctx, userID)
	if err != nil || none != nil {
		t.Fatalf("no goal yet: %v %v", none, err)
	}
	exam := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	created, err := s.Create(ctx, CreateInput{UserID: userID, Title: "  GATE  ", ExamDate: exam, DailyMinutes: 90,
		SubjectIDs: []string{subjectID, subjectID}, CurriculumID: curriculumID})
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "GATE" || created.DaysRemaining != 30 || created.TotalMinutesAvailable != 2700 || len(created.SubjectIDs) != 1 {
		t.Errorf("created %+v", created)
	}
	got, err := s.GetActive(ctx, userID)
	if err != nil || got == nil || got.ID != created.ID || got.ExamDate != exam || *got.CurriculumID != curriculumID || len(got.SubjectIDs) != 1 {
		t.Errorf("active %+v %v", got, err)
	}
	subjects, err := s.ListSubjects(ctx, curriculumID)
	if err != nil || len(subjects) == 0 {
		t.Errorf("subjects %v %v", subjects, err)
	}
}
