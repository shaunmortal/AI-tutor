package store

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/groups"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// GroupRow is one study group.
type GroupRow struct {
	ID           string    `db:"id" json:"id"`
	Name         string    `db:"name" json:"name"`
	InviteCode   string    `db:"invite_code" json:"invite_code"`
	CurriculumID *string   `db:"curriculum_id" json:"curriculum_id"`
	CreatedBy    *string   `db:"created_by" json:"created_by"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// MyGroup is a group the learner belongs to, with their role and the member count.
type MyGroup struct {
	Group       GroupRow `json:"group"`
	Role        string   `json:"role"`
	MemberCount int      `json:"memberCount"`
}

// GroupMember is a member id and role.
type GroupMember struct {
	UserID string `db:"user_id" json:"userId"`
	Role   string `db:"role" json:"role"`
}

// NewGroup is a group to create.
type NewGroup struct {
	Name         string
	InviteCode   string
	CurriculumID *string
	CreatedBy    string
}

const groupCols = `id, name, invite_code, curriculum_id, created_by, created_at`

// Groups holds study groups and membership. Writes need Admin: a learner able to insert into
// group_members directly could join any group whose id they learned.
//
// The peer-progress reads are the only place one learner's request touches another's rows.
// Their select lists are the privacy boundary: a field not wanted on a peer's screen is never
// fetched.
type Groups struct{ q Q }

func NewGroups(q Q) *Groups { return &Groups{q} }

// FindMyGroups is the groups this learner belongs to, with their role.
func (r *Groups) FindMyGroups(ctx context.Context, userID string) ([]MyGroup, error) {
	type row struct {
		GroupRow
		Role        string `db:"role"`
		MemberCount int    `db:"member_count"`
	}
	rows, err := rowsOf[row](ctx, r.q, "groups.findMyGroups",
		`select g.id, g.name, g.invite_code, g.curriculum_id, g.created_by, g.created_at, m.role,
		        (select count(*)::int from group_members c where c.group_id = g.id) as member_count
		 from group_members m join study_groups g on g.id = m.group_id
		 where m.user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	out := make([]MyGroup, len(rows))
	for i, g := range rows {
		count := g.MemberCount
		if count == 0 {
			count = 1
		}
		out[i] = MyGroup{Group: g.GroupRow, Role: g.Role, MemberCount: count}
	}
	return out, nil
}

func (r *Groups) FindByInviteCode(ctx context.Context, code string) (*GroupRow, error) {
	return rowOf[GroupRow](ctx, r.q, "groups.findByInviteCode",
		`select `+groupCols+` from study_groups where invite_code = $1`, code)
}

func (r *Groups) FindGroup(ctx context.Context, groupID string) (*GroupRow, error) {
	return rowOf[GroupRow](ctx, r.q, "groups.findGroup",
		`select `+groupCols+` from study_groups where id = $1`, groupID)
}

// IsMember is the gate in front of every peer read.
func (r *Groups) IsMember(ctx context.Context, groupID, userID string) (bool, error) {
	found, err := colOf[bool](ctx, r.q, "groups.isMember",
		`select true from group_members where group_id = $1 and user_id = $2`, groupID, userID)
	return len(found) > 0, err
}

func (r *Groups) CreateGroup(ctx context.Context, g NewGroup) (*GroupRow, error) {
	return single[GroupRow](ctx, r.q, "groups.createGroup",
		`insert into study_groups (name, invite_code, curriculum_id, created_by) values ($1, $2, $3, $4)
		 returning `+groupCols, g.Name, g.InviteCode, g.CurriculumID, g.CreatedBy)
}

// AddMember is idempotent (already a member is not an error) and turns the group-size
// trigger's refusal into 409 GROUP_FULL.
func (r *Groups) AddMember(ctx context.Context, groupID, userID, role string) error {
	_, err := r.q.Exec(ctx,
		`insert into group_members (group_id, user_id, role) values ($1, $2, $3) on conflict do nothing`,
		groupID, userID, role)
	if err != nil && strings.Contains(err.Error(), "this group is full") {
		return httpx.E(http.StatusConflict, "GROUP_FULL", "That group is full. Ask whoever runs it to make another one.")
	}
	if err != nil {
		return wrap("groups.addMember", err)
	}
	return nil
}

func (r *Groups) RemoveMember(ctx context.Context, groupID, userID string) error {
	return exec(ctx, r.q, "groups.removeMember",
		`delete from group_members where group_id = $1 and user_id = $2`, groupID, userID)
}

// FindMemberIDs is a group's members in join order.
func (r *Groups) FindMemberIDs(ctx context.Context, groupID string) ([]GroupMember, error) {
	return rowsOf[GroupMember](ctx, r.q, "groups.findMemberIds",
		`select user_id, role from group_members where group_id = $1 order by joined_at`, groupID)
}

// FindSharedProgress is the four shared numbers for each of userIDs, keyed by user id
// (IsYou is left false for the caller to set). Display name only — never email — and no
// accuracy or per-topic scores.
func (r *Groups) FindSharedProgress(ctx context.Context, userIDs []string, masteryBar float64) (map[string]groups.SharedProgress, error) {
	out := map[string]groups.SharedProgress{}
	if len(userIDs) == 0 {
		return out, nil
	}
	type row struct {
		ID                string  `db:"id"`
		DisplayName       *string `db:"display_name"`
		TotalAttempts     *int    `db:"total_attempts"`
		CurrentStreakDays *int    `db:"current_streak_days"`
		TopicsMastered    int     `db:"topics_mastered"`
	}
	rows, err := rowsOf[row](ctx, r.q, "groups.findSharedProgress",
		`select u.id, p.display_name, lp.total_attempts, lp.current_streak_days,
		        (select count(*)::int from concept_mastery m where m.user_id = u.id and m.mastery_score >= $2) as topics_mastered
		 from unnest($1::uuid[]) as u(id)
		 left join profiles p on p.id = u.id
		 left join learner_profiles lp on lp.user_id = u.id`, userIDs, masteryBar)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		// A learner who never set a name is "Learner", not a raw uuid.
		name := "Learner"
		if p.DisplayName != nil && strings.TrimSpace(*p.DisplayName) != "" {
			name = strings.TrimSpace(*p.DisplayName)
		}
		sp := groups.SharedProgress{UserID: p.ID, DisplayName: name, TopicsMastered: p.TopicsMastered}
		if p.TotalAttempts != nil {
			sp.QuestionsAnswered = *p.TotalAttempts
		}
		if p.CurrentStreakDays != nil {
			sp.CurrentStreakDays = *p.CurrentStreakDays
		}
		out[p.ID] = sp
	}
	return out, nil
}
