package store

import (
	"context"

	"github.com/shaunmortal/AI-tutor/server/internal/curriculum"
)

// CurriculumRow is one syllabus.
type CurriculumRow struct {
	ID            string  `db:"id" json:"id"`
	Slug          string  `db:"slug" json:"slug"`
	Name          string  `db:"name" json:"name"`
	Description   *string `db:"description" json:"description"`
	IsAIGenerated bool    `db:"is_ai_generated" json:"is_ai_generated"`
}

// SubjectRow is a top-level topic with how many topics sit under it.
type SubjectRow struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Weight     float64 `json:"weight"`
	TopicCount int     `json:"topicCount"`
}

const curriculumCols = `id, slug, name, description, is_ai_generated`

// DefaultCurriculumSlug is the syllabus that ships with the app.
const DefaultCurriculumSlug = "computer-science-fundamentals"

// Curricula holds curricula and their subject trees. Reads work with either Q; writes need
// Admin, because `curricula` and `topics` grant learners SELECT only.
type Curricula struct{ q Q }

func NewCurricula(q Q) *Curricula { return &Curricula{q} }

func (r *Curricula) FindBySlug(ctx context.Context, slug string) (*CurriculumRow, error) {
	return rowOf[CurriculumRow](ctx, r.q, "curricula.findBySlug",
		`select `+curriculumCols+` from curricula where slug = $1`, slug)
}

func (r *Curricula) FindByID(ctx context.Context, id string) (*CurriculumRow, error) {
	return rowOf[CurriculumRow](ctx, r.q, "curricula.findById",
		`select `+curriculumCols+` from curricula where id = $1`, id)
}

// FindDefault is the default syllabus, used when a learner has no goal yet.
func (r *Curricula) FindDefault(ctx context.Context) (*CurriculumRow, error) {
	return r.FindBySlug(ctx, DefaultCurriculumSlug)
}

// FindSubjects lists one curriculum's subjects in sort order, each with its topic count.
func (r *Curricula) FindSubjects(ctx context.Context, curriculumID string) ([]SubjectRow, error) {
	type row struct {
		ID       string  `db:"id"`
		Name     string  `db:"name"`
		Weight   float64 `db:"weight"`
		ParentID *string `db:"parent_id"`
	}
	rows, err := rowsOf[row](ctx, r.q, "curricula.findSubjects",
		`select id, name, weight, parent_id from topics where curriculum_id = $1 order by sort_order`, curriculumID)
	if err != nil {
		return nil, err
	}

	childCount := map[string]int{}
	for _, t := range rows {
		if t.ParentID != nil {
			childCount[*t.ParentID]++
		}
	}
	out := []SubjectRow{}
	for _, t := range rows {
		if t.ParentID == nil {
			out = append(out, SubjectRow{ID: t.ID, Name: t.Name, Weight: t.Weight, TopicCount: childCount[t.ID]})
		}
	}
	return out, nil
}

// Create writes a validated syllabus: the curriculum, then its subjects, then their topics.
// Three statements, as the TS made three requests; pass a transaction as q to make it atomic.
func (r *Curricula) Create(ctx context.Context, c curriculum.Resolved) (*CurriculumRow, error) {
	var description *string
	if c.Description != "" {
		description = &c.Description
	}
	created, err := single[CurriculumRow](ctx, r.q, "curricula.create",
		`insert into curricula (slug, name, description, is_ai_generated) values ($1, $2, $3, true)
		 returning `+curriculumCols, c.Slug, c.Name, description)
	if err != nil {
		return nil, err
	}

	// sort_order carries the order the model returned, which is the order a syllabus is taught in.
	names := make([]string, len(c.Subjects))
	weights := make([]float64, len(c.Subjects))
	for i, s := range c.Subjects {
		names[i], weights[i] = s.Name, s.Weight
	}
	type idName struct {
		ID   string `db:"id"`
		Name string `db:"name"`
	}
	subjects, err := rowsOf[idName](ctx, r.q, "curricula.create",
		`insert into topics (curriculum_id, parent_id, name, weight, sort_order)
		 select $1, null, n, w, o from unnest($2::text[], $3::numeric[]) with ordinality as t(n, w, o)
		 returning id, name`, created.ID, names, weights)
	if err != nil {
		return nil, err
	}
	subjectIDByName := map[string]string{}
	for _, s := range subjects {
		subjectIDByName[s.Name] = s.ID
	}

	var parents, topicNames []string
	var topicWeights []float64
	var orders []int
	for _, s := range c.Subjects {
		parentID, ok := subjectIDByName[s.Name]
		if !ok {
			continue
		}
		for i, topic := range s.Topics {
			// Topics inherit their subject's weight until evidence says otherwise.
			parents = append(parents, parentID)
			topicNames = append(topicNames, topic)
			topicWeights = append(topicWeights, s.Weight)
			orders = append(orders, i+1)
		}
	}
	if len(parents) > 0 {
		if err := exec(ctx, r.q, "curricula.create",
			`insert into topics (curriculum_id, parent_id, name, weight, sort_order)
			 select $1, p, n, w, o from unnest($2::uuid[], $3::text[], $4::numeric[], $5::int[]) as t(p, n, w, o)`,
			created.ID, parents, topicNames, topicWeights, orders); err != nil {
			return nil, err
		}
	}
	return created, nil
}
