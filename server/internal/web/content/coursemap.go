package content

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/curriculum"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

const (
	// How much of each excerpt the course-map call sees. Enough to recognise the topic.
	mapPreviewChars = 220
	// Upper bound on the course-map prompt, so a 600-page book still fits in one request.
	mapMaxExcerpts = 450
)

// locationOf is what a citation chip shows: "p. 42", "slide 7" or "12:05".
func locationOf(c store.ChunkRow) string {
	switch {
	case c.StartSec != nil:
		return clock(*c.StartSec)
	case c.Slide != nil:
		return "slide " + strconv.Itoa(*c.Slide)
	case c.Page != nil:
		return "p. " + strconv.Itoa(*c.Page)
	}
	return "p. null" // as the TS printed it; the schema requires a location, so unreachable
}

func clock(total int) string {
	h, m, s := total/3600, total%3600/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

type mapTopic struct {
	Name          string
	KeyConcepts   []string
	Prerequisites []string
	ExcerptIDs    []int
}

type mapSubject struct {
	Name   string
	Weight int
	Topics []mapTopic
}

func malformedMap(msg string) error { return httpx.E(http.StatusBadGateway, "AI_MALFORMED", msg) }

// parseCourseMap checks the model's reply rather than trusting it: it becomes topics other
// features act on. Decoded loosely (any) so a wrong type degrades the way the TS's did.
func parseCourseMap(raw any) ([]mapSubject, error) {
	obj, _ := raw.(map[string]any)
	subjects, _ := obj["subjects"].([]any)
	if len(subjects) == 0 {
		return nil, malformedMap("The course map came back empty. Try again.")
	}
	seenTopics, seenSubjects := map[string]bool{}, map[string]bool{}
	var all []mapSubject
	// Topic names must be unique across the course (they are how prerequisites refer), and
	// are claimed in order even by a subject that is dropped later.
	for _, rs := range subjects {
		so, _ := rs.(map[string]any)
		sub := mapSubject{Name: clip(jsString(so["name"]), 120), Weight: clampWeight(so["weight"])}
		topics, _ := so["topics"].([]any)
		for _, rt := range topics {
			to, _ := rt.(map[string]any)
			t := mapTopic{Name: clip(jsString(to["name"]), 120), KeyConcepts: []string{}, Prerequisites: []string{}, ExcerptIDs: []int{}}
			for _, v := range arr(to["keyConcepts"]) {
				if s := jsString(v); s != "" {
					t.KeyConcepts = append(t.KeyConcepts, s)
				}
			}
			for _, v := range arr(to["prerequisites"]) {
				t.Prerequisites = append(t.Prerequisites, jsString(v))
			}
			for _, v := range arr(to["excerptIds"]) {
				if f, ok := v.(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
					t.ExcerptIDs = append(t.ExcerptIDs, int(f))
				}
			}
			if t.Name != "" && !seenTopics[t.Name] {
				seenTopics[t.Name] = true
				sub.Topics = append(sub.Topics, t)
			}
		}
		all = append(all, sub)
	}
	var cleaned []mapSubject
	for _, sub := range all {
		if sub.Name != "" && len(sub.Topics) > 0 && !seenSubjects[sub.Name] {
			seenSubjects[sub.Name] = true
			cleaned = append(cleaned, sub)
		}
	}
	if len(cleaned) == 0 {
		return nil, malformedMap("The course map had no usable topics. Try again.")
	}
	return cleaned, nil
}

func arr(v any) []any { a, _ := v.([]any); return a }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}

// jsString is String(v ?? ”) for JSON values.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

// clampWeight is min(5, max(1, round(Number(w) || 1))).
func clampWeight(v any) int {
	f := 0.0
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		f, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
	case bool:
		if x {
			f = 1
		}
	}
	if f == 0 || math.IsNaN(f) {
		f = 1
	}
	return int(min(5, max(1, math.Floor(f+0.5))))
}

// assignTopics gives every excerpt a topic: the one the model chose, or, for excerpts it was not
// shown, the topic of the nearest shown excerpt in the same source. Neighbouring pages of a book
// are almost always on the same topic, so this is right far more often than wrong, and it is free.
func assignTopics(chunks []store.ChunkRow, topicOfIndex map[int]string) []store.ChunkAssignment {
	var out []store.ChunkAssignment
	for i, c := range chunks {
		if t, ok := topicOfIndex[i]; ok {
			out = append(out, store.ChunkAssignment{ChunkID: c.ID, TopicID: t})
			continue
		}
	search:
		for d := 1; d < len(chunks); d++ {
			for _, n := range []int{i - d, i + d} {
				if t, ok := topicOfIndex[n]; ok && n >= 0 && n < len(chunks) && chunks[n].SourceID == c.SourceID {
					out = append(out, store.ChunkAssignment{ChunkID: c.ID, TopicID: t})
					break search
				}
			}
		}
	}
	return out
}

var whitespace = regexp.MustCompile(`\s+`)

// organize groups every ready excerpt into subjects and topics, with concepts and prerequisites,
// and writes it as a curriculum the rest of the app already plans, tests and tracks.
//
// Each call writes a new curriculum rather than editing the last: topic ids carry mastery, and
// silently re-pointing them would corrupt a learner's history.
func (s *svc) organize(ctx context.Context, userID, courseName string) (*courseMapView, error) {
	repo := s.sources()
	chunks, err := repo.ListChunks(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, httpx.E(http.StatusConflict, "NO_MATERIAL", "Add at least one source and wait for it to finish processing first.")
	}
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.SourceID
	}
	srcs, err := repo.FindMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	titles := map[string]string{}
	for _, src := range srcs {
		titles[src.ID] = src.Title
	}

	// Evenly spaced when there is more than fits, so every part of every source is represented.
	step := max(1, float64(len(chunks))/mapMaxExcerpts)
	var shown []ai.MapExcerpt
	for pos := 0.0; pos < float64(len(chunks)); pos += step {
		i := int(pos)
		c := chunks[i]
		label, ok := titles[c.SourceID]
		if !ok {
			label = "source"
		}
		text := []rune(whitespace.ReplaceAllString(c.Content, " "))
		shown = append(shown, ai.MapExcerpt{ID: i, Label: label + ", " + locationOf(c), Text: string(text[:min(len(text), mapPreviewChars)])})
	}

	var raw any
	if _, err := s.d.AI.JSON(ctx, ai.Request{System: ai.CourseMapSystem, Prompt: ai.CourseMapPrompt(courseName, shown),
		Schema: ai.CourseMapSchema, Quality: ai.High, MaxTokens: 16000}, &raw); err != nil {
		return nil, err
	}
	subjects, err := parseCourseMap(raw)
	if err != nil {
		return nil, err
	}

	slug := curriculum.ToSlug(courseName)
	if r := []rune(slug); len(r) > 60 {
		slug = string(r[:60])
	}
	if slug == "" {
		slug = "course"
	}
	plural := "s"
	if len(titles) == 1 {
		plural = ""
	}
	resolved := curriculum.Resolved{
		Slug:        slug + "-" + strings.ToLower(rand.Text()[:8]),
		Name:        courseName,
		Description: fmt.Sprintf("Built from %d uploaded source%s.", len(titles), plural),
	}
	for _, sub := range subjects {
		rs := curriculum.ResolvedSubject{Name: sub.Name, Weight: float64(sub.Weight)}
		for _, t := range sub.Topics {
			rs.Topics = append(rs.Topics, t.Name)
		}
		resolved.Subjects = append(resolved.Subjects, rs)
	}

	// One transaction: a half-written course map is worse than none.
	var curriculumID string
	err = pgx.BeginFunc(ctx, s.d.DB.Admin, func(tx pgx.Tx) error {
		created, err := store.NewCurricula(tx).Create(ctx, resolved)
		if err != nil {
			return err
		}
		curriculumID = created.ID
		r := store.NewSources(tx)
		cm, err := r.CourseMap(ctx, created.ID, userID)
		if err != nil {
			return err
		}
		topicIDByName := map[string]string{}
		for _, t := range cm.Topics {
			if t.ParentID != nil {
				topicIDByName[t.Name] = t.ID
			}
		}
		topicOfIndex := map[int]string{}
		var pairs []store.Prerequisite
		for _, sub := range subjects {
			for _, t := range sub.Topics {
				topicID, ok := topicIDByName[t.Name]
				if !ok {
					continue
				}
				if err := r.SetTopicDetails(ctx, topicID, t.KeyConcepts[:min(8, len(t.KeyConcepts))]); err != nil {
					return err
				}
				for _, id := range t.ExcerptIDs {
					topicOfIndex[id] = topicID
				}
				for _, name := range t.Prerequisites {
					if pre, ok := topicIDByName[name]; ok && pre != topicID {
						pairs = append(pairs, store.Prerequisite{TopicID: topicID, PrerequisiteID: pre})
					}
				}
			}
		}
		if err := r.AddPrerequisites(ctx, pairs); err != nil {
			return err
		}
		if err := r.TagChunks(ctx, assignTopics(chunks, topicOfIndex)); err != nil {
			return err
		}
		return r.AttachCurriculum(ctx, userID, created.ID)
	})
	if err != nil {
		return nil, err
	}
	return s.courseMap(ctx, curriculumID, userID)
}

type namedRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type topicView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	KeyConcepts   []string   `json:"keyConcepts"`
	ExcerptCount  int        `json:"excerptCount"`
	Prerequisites []namedRef `json:"prerequisites"`
}

type subjectView struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Topics []topicView `json:"topics"`
}

type courseMapView struct {
	Curriculum *namedRef     `json:"curriculum"`
	Subjects   []subjectView `json:"subjects"`
}

// courseMap is a curriculum's subjects, topics, concepts, prerequisites and material per topic.
func (s *svc) courseMap(ctx context.Context, curriculumID, userID string) (*courseMapView, error) {
	m, err := s.sources().CourseMap(ctx, curriculumID, userID)
	if err != nil {
		return nil, err
	}
	c, err := store.NewCurricula(s.d.DB.Admin).FindByID(ctx, curriculumID)
	if err != nil {
		return nil, err
	}
	out := &courseMapView{Subjects: []subjectView{}}
	if c != nil {
		out.Curriculum = &namedRef{c.ID, c.Name}
	}
	nameOf := map[string]string{}
	for _, t := range m.Topics {
		nameOf[t.ID] = t.Name
	}
	for _, sub := range m.Topics {
		if sub.ParentID != nil {
			continue
		}
		sv := subjectView{ID: sub.ID, Name: sub.Name, Topics: []topicView{}}
		for _, t := range m.Topics {
			if t.ParentID == nil || *t.ParentID != sub.ID {
				continue
			}
			tv := topicView{ID: t.ID, Name: t.Name, KeyConcepts: t.KeyConcepts, ExcerptCount: m.ExcerptCounts[t.ID], Prerequisites: []namedRef{}}
			if tv.KeyConcepts == nil {
				tv.KeyConcepts = []string{}
			}
			for _, p := range m.Prerequisites {
				if p.TopicID == t.ID {
					tv.Prerequisites = append(tv.Prerequisites, namedRef{p.PrerequisiteID, nameOf[p.PrerequisiteID]})
				}
			}
			sv.Topics = append(sv.Topics, tv)
		}
		out.Subjects = append(out.Subjects, sv)
	}
	return out, nil
}
