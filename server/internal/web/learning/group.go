package learning

// Study groups (groupService.ts). Every endpoint uses Admin: group tables grant learners SELECT
// only, so joining has to come through here or the invite code is decoration. Admin also
// bypasses RLS, which is why assertMember runs before every read of another learner's numbers.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/groups"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

const (
	// A study group is classmates, not a subscription list.
	maxGroupsPerLearner = 10
	// Attempts at a unique invite code; collisions are vanishingly rare.
	codeAttempts = 5
)

type groupSummary struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	InviteCode  string    `json:"inviteCode"`
	MemberCount int       `json:"memberCount"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"createdAt"`
}

type groupDetail struct {
	groupSummary
	Comparison groups.Comparison `json:"comparison"`
}

func toSummary(g *store.GroupRow, role string, memberCount int) groupSummary {
	return groupSummary{g.ID, g.Name, g.InviteCode, memberCount, role, g.CreatedAt}
}

var errGroupNotFound = httpx.E(http.StatusNotFound, "GROUP_NOT_FOUND", "That group does not exist.")

// assertMember is the gate: the group, only if this learner is in it. 404 rather than 403 so
// the id is not confirmed.
func assertMember(ctx context.Context, g *store.Groups, userID, groupID string) (*store.GroupRow, error) {
	group, err := g.FindGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, errGroupNotFound
	}
	member, err := g.IsMember(ctx, groupID, userID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, errGroupNotFound
	}
	return group, nil
}

func assertRoomForAnotherGroup(ctx context.Context, g *store.Groups, userID string) error {
	mine, err := g.FindMyGroups(ctx, userID)
	if err != nil {
		return err
	}
	if len(mine) >= maxGroupsPerLearner {
		return httpx.E(http.StatusConflict, "TOO_MANY_GROUPS",
			fmt.Sprintf("You are already in %d groups. Leave one first.", maxGroupsPerLearner))
	}
	return nil
}

// createWithUniqueCode inserts with a fresh code and retries on the unique index rather than
// checking first, which would race.
func createWithUniqueCode(ctx context.Context, g *store.Groups, userID, name string, curriculumID *string) (*store.GroupRow, error) {
	for attempt := 0; ; attempt++ {
		group, err := g.CreateGroup(ctx, store.NewGroup{
			Name:         name,
			InviteCode:   groups.MakeInviteCode(func(n int) []byte { b := make([]byte, n); _, _ = rand.Read(b); return b }),
			CurriculumID: curriculumID,
			CreatedBy:    userID,
		})
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" || attempt == codeAttempts-1 {
			return group, err
		}
	}
}

// groupID is the {id} path value, or the same 404 as a group that is not yours.
func groupID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !isUUID(id) {
		return "", errGroupNotFound
	}
	return id, nil
}

// GET /groups — the groups this learner is in.
func (h handlers) listGroups(w http.ResponseWriter, r *http.Request) error {
	rows, err := store.NewGroups(h.d.DB.Admin).FindMyGroups(r.Context(), auth.UserID(r))
	if err != nil {
		return err
	}
	out := make([]groupSummary, len(rows))
	for i, row := range rows {
		out[i] = toSummary(&row.Group, row.Role, row.MemberCount)
	}
	httpx.OK(w, http.StatusOK, map[string]any{"groups": out})
	return nil
}

// POST /groups — the syllabus comes from the creator's own goal (read as the learner).
func (h handlers) createGroup(w http.ResponseWriter, r *http.Request) error {
	f, err := readObject(w, r)
	if err != nil {
		return err
	}
	name, err := f.str("name", 2, 60)
	if err != nil {
		return err
	}
	goal, err := h.activeGoal(r)
	if err != nil {
		return err
	}
	var curriculumID *string
	if goal != nil {
		curriculumID = goal.CurriculumID
	}

	ctx, userID, g := r.Context(), auth.UserID(r), store.NewGroups(h.d.DB.Admin)
	if err := assertRoomForAnotherGroup(ctx, g, userID); err != nil {
		return err
	}
	group, err := createWithUniqueCode(ctx, g, userID, name, curriculumID)
	if err != nil {
		return err
	}
	if err := g.AddMember(ctx, group.ID, userID, "owner"); err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"group": toSummary(group, "owner", 1)})
	return nil
}

// POST /groups/join — any code that does not resolve gets the same answer, so the endpoint
// cannot be used to probe which codes exist.
func (h handlers) joinGroup(w http.ResponseWriter, r *http.Request) error {
	f, err := readObject(w, r)
	if err != nil {
		return err
	}
	raw, err := f.str("inviteCode", 4, 20)
	if err != nil {
		return err
	}
	code := groups.NormaliseInviteCode(raw)
	if len(code) != groups.InviteCodeLength {
		return httpx.E(http.StatusBadRequest, "INVALID_CODE", "An invite code is six characters. Check it and try again.")
	}

	ctx, userID, g := r.Context(), auth.UserID(r), store.NewGroups(h.d.DB.Admin)
	group, err := g.FindByInviteCode(ctx, code)
	if err != nil {
		return err
	}
	if group == nil {
		return httpx.E(http.StatusNotFound, "GROUP_NOT_FOUND", "No group with that code. Check it with whoever sent it.")
	}
	member, err := g.IsMember(ctx, group.ID, userID)
	if err != nil {
		return err
	}
	if !member {
		if err := assertRoomForAnotherGroup(ctx, g, userID); err != nil {
			return err
		}
	}
	// Idempotent, so pasting a code twice is not an error.
	if err := g.AddMember(ctx, group.ID, userID, "member"); err != nil {
		return err
	}
	members, err := g.FindMemberIDs(ctx, group.ID)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"group": toSummary(group, "member", len(members))})
	return nil
}

// GET /groups/{id} — members and the ranked comparison, members only.
func (h handlers) getGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := groupID(r)
	if err != nil {
		return err
	}
	ctx, userID, g := r.Context(), auth.UserID(r), store.NewGroups(h.d.DB.Admin)
	group, err := assertMember(ctx, g, userID, id)
	if err != nil {
		return err
	}
	members, err := g.FindMemberIDs(ctx, id)
	if err != nil {
		return err
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.UserID
	}
	progress, err := g.FindSharedProgress(ctx, ids, learner.MasteryBar)
	if err != nil {
		return err
	}

	role := "member"
	shared := make([]groups.SharedProgress, len(members))
	for i, m := range members {
		p, ok := progress[m.UserID]
		if !ok {
			p = groups.SharedProgress{DisplayName: "Learner"}
		}
		p.UserID, p.IsYou = m.UserID, m.UserID == userID
		shared[i] = p
		if p.IsYou {
			role = m.Role
		}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"group": groupDetail{
		toSummary(group, role, len(members)), groups.RankMembers(shared),
	}})
	return nil
}

// POST /groups/{id}/leave — the last member out leaves an empty group, which is harmless.
func (h handlers) leaveGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := groupID(r)
	if err != nil {
		return err
	}
	ctx, userID, g := r.Context(), auth.UserID(r), store.NewGroups(h.d.DB.Admin)
	if _, err := assertMember(ctx, g, userID, id); err != nil {
		return err
	}
	if err := g.RemoveMember(ctx, id, userID); err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"left": true})
	return nil
}
