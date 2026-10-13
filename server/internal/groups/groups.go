// Package groups decides what a study group is allowed to say about its members, and in
// what order.
//
// The interesting decision is the shape of SharedProgress: it is the privacy boundary of the
// whole feature, the complete list of what one learner can learn about another, and it is a
// type rather than a convention so that adding a field is a visible change here rather than
// an extra column slipped into a query.
//
// Shared: name, questions answered, current streak, topics mastered. Not shared: accuracy,
// weakest topics, wrong answers, per-topic mastery, tutor conversations, goals, plans. The
// cut is effort and achievement on one side, weakness on the other — publishing which topics
// somebody is worst at, to their classmates, is a reason to stop using the app.
package groups

import (
	"cmp"
	"slices"
	"strings"
)

// SharedProgress is everything one group member may know about another.
type SharedProgress struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	// Lifetime questions answered. Effort, not correctness.
	QuestionsAnswered int `json:"questionsAnswered"`
	CurrentStreakDays int `json:"currentStreakDays"`
	TopicsMastered    int `json:"topicsMastered"`
	// True for the learner reading the screen, so the client can mark their own row.
	IsYou bool `json:"isYou"`
}

// RankedMember is a member with their place on the board.
type RankedMember struct {
	SharedProgress
	// 1-based. Ties share a rank, so two learners on 40 questions are both 3rd.
	Rank int `json:"rank"`
}

// Totals are the group sums — the part that makes it collaborative, not only competitive.
type Totals struct {
	QuestionsAnswered int `json:"questionsAnswered"`
	TopicsMastered    int `json:"topicsMastered"`
}

// TopStreak is the longest current streak in the group, and who holds it.
type TopStreak struct {
	DisplayName string `json:"displayName"`
	Days        int    `json:"days"`
}

// Comparison is the group leaderboard.
type Comparison struct {
	Members []RankedMember `json:"members"`
	// Where the reader sits, for the "you are 3rd of 6" line.
	YourRank    *int       `json:"yourRank"`
	MemberCount int        `json:"memberCount"`
	Totals      Totals     `json:"totals"`
	TopStreak   *TopStreak `json:"topStreak"`
}

// RankMembers ranks a group by questions answered.
//
// Effort rather than accuracy, deliberately: ranking on accuracy would reward answering only
// easy questions and punish the learner working through the topic they find hardest. Ties
// share a rank — inventing an order between identical numbers would be arbitrary and, on a
// screen people compare themselves on, unkind for no reason.
func RankMembers(members []SharedProgress) Comparison {
	sorted := slices.Clone(members)
	slices.SortStableFunc(sorted, func(a, b SharedProgress) int {
		return cmp.Or(
			cmp.Compare(b.QuestionsAnswered, a.QuestionsAnswered),
			cmp.Compare(b.TopicsMastered, a.TopicsMastered),
			cmp.Compare(b.CurrentStreakDays, a.CurrentStreakDays),
			// Final tiebreak on name so the order is stable between reads rather than
			// depending on whatever order the database returned.
			localeCompare(a.DisplayName, b.DisplayName),
		)
	})

	c := Comparison{Members: make([]RankedMember, 0, len(sorted)), MemberCount: len(sorted)}
	for i, m := range sorted {
		rank := i + 1
		if i > 0 && m.QuestionsAnswered == sorted[i-1].QuestionsAnswered {
			rank = c.Members[i-1].Rank
		}
		c.Members = append(c.Members, RankedMember{m, rank})
		c.Totals.QuestionsAnswered += m.QuestionsAnswered
		c.Totals.TopicsMastered += m.TopicsMastered

		if m.IsYou && c.YourRank == nil {
			c.YourRank = &rank
		}
		// Strictly greater, so the first in board order wins a tied streak.
		if m.CurrentStreakDays > 0 && (c.TopStreak == nil || m.CurrentStreakDays > c.TopStreak.Days) {
			c.TopStreak = &TopStreak{m.DisplayName, m.CurrentStreakDays}
		}
	}
	return c
}

// Alphabet for invite codes. No O, 0, I, 1 or L: these codes get read aloud across a room,
// and an ambiguous character means a learner who cannot join and has no idea why.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// InviteCodeLength is the length of a group invite code.
const InviteCodeLength = 6

// MakeInviteCode generates an invite code from randomBytes, which is passed in so tests can
// feed a known sequence (use crypto/rand in production). Uses rejection sampling — taking
// byte % 31 would make the first symbols of the alphabet slightly likelier.
func MakeInviteCode(randomBytes func(n int) []byte) string {
	limit := 256 / len(codeAlphabet) * len(codeAlphabet)
	code := make([]byte, 0, InviteCodeLength)

	for len(code) < InviteCodeLength {
		// Ask for more than needed so a run of rejected bytes rarely needs a second call.
		for _, b := range randomBytes(InviteCodeLength * 2) {
			if int(b) >= limit {
				continue
			}
			code = append(code, codeAlphabet[int(b)%len(codeAlphabet)])
			if len(code) == InviteCodeLength {
				break
			}
		}
	}
	return string(code)
}

// NormaliseInviteCode tidies a typed code: people type lower case and paste spaces.
func NormaliseInviteCode(input string) string {
	return strings.Join(strings.Fields(strings.ToUpper(input)), "")
}

// localeCompare approximates JavaScript's localeCompare (ICU root collation) for names:
// case-insensitive first, then lower case before upper case. It does not reproduce ICU's
// ordering of accents or punctuation.
func localeCompare(a, b string) int {
	return cmp.Or(strings.Compare(strings.ToLower(a), strings.ToLower(b)), strings.Compare(b, a))
}
