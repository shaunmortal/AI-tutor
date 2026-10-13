package groups

import (
	"math/rand/v2"
	"regexp"
	"slices"
	"testing"
)

// The ranking rules are opinions with consequences — rank on the wrong number and the app
// starts rewarding the wrong behaviour — so they are pinned here.

func member(mod func(*SharedProgress)) SharedProgress {
	m := SharedProgress{UserID: "u1", DisplayName: "Learner", QuestionsAnswered: 10, CurrentStreakDays: 1}
	if mod != nil {
		mod(&m)
	}
	return m
}

func m(id, name string, questions int) SharedProgress {
	return member(func(p *SharedProgress) { p.UserID, p.DisplayName, p.QuestionsAnswered = id, name, questions })
}

func pluck[T any](c Comparison, get func(RankedMember) T) []T {
	out := []T{}
	for _, r := range c.Members {
		out = append(out, get(r))
	}
	return out
}

func TestRankByQuestionsAnswered(t *testing.T) {
	c := RankMembers([]SharedProgress{m("a", "Asha", 40), m("b", "Bilal", 90), m("c", "Chetan", 60)})
	if got := pluck(c, func(r RankedMember) string { return r.DisplayName }); !slices.Equal(got, []string{"Bilal", "Chetan", "Asha"}) {
		t.Errorf("names = %v", got)
	}
	if got := pluck(c, func(r RankedMember) int { return r.Rank }); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("ranks = %v", got)
	}
}

// Effort, not accuracy — the test that stops somebody "improving" it later.
func TestDoesNotRewardCherryPicking(t *testing.T) {
	grinder := member(func(p *SharedProgress) {
		p.UserID, p.DisplayName, p.QuestionsAnswered, p.TopicsMastered = "a", "Asha", 200, 1
	})
	cherryPicker := member(func(p *SharedProgress) {
		p.UserID, p.DisplayName, p.QuestionsAnswered, p.TopicsMastered = "b", "Bilal", 20, 1
	})
	if got := RankMembers([]SharedProgress{cherryPicker, grinder}).Members[0].DisplayName; got != "Asha" {
		t.Fatalf("first = %s", got)
	}
}

func TestTiesShareARank(t *testing.T) {
	c := RankMembers([]SharedProgress{m("a", "Asha", 50), m("b", "Bilal", 50), m("c", "Chetan", 10)})
	if got := pluck(c, func(r RankedMember) int { return r.Rank }); !slices.Equal(got, []int{1, 1, 3}) {
		t.Fatalf("ranks = %v", got)
	}
}

func TestIdenticalMembersOrderedTheSameEveryRead(t *testing.T) {
	people := []SharedProgress{m("b", "Bilal", 10), m("a", "Asha", 10), m("c", "Chetan", 10)}
	reversed := slices.Clone(people)
	slices.Reverse(reversed)
	id := func(r RankedMember) string { return r.UserID }
	if first, again := pluck(RankMembers(people), id), pluck(RankMembers(reversed), id); !slices.Equal(first, again) {
		t.Fatalf("%v != %v", first, again)
	}
}

func TestReportsWhereTheReaderSits(t *testing.T) {
	you := m("b", "You", 30)
	you.IsYou = true
	c := RankMembers([]SharedProgress{m("a", "Asha", 90), you, m("c", "Chetan", 60)})
	if c.YourRank == nil || *c.YourRank != 3 || c.MemberCount != 3 {
		t.Fatalf("yourRank %v memberCount %d", c.YourRank, c.MemberCount)
	}
}

func TestNoRankForNonMember(t *testing.T) {
	if c := RankMembers([]SharedProgress{member(func(p *SharedProgress) { p.UserID = "a" })}); c.YourRank != nil {
		t.Fatalf("yourRank = %d", *c.YourRank)
	}
}

func TestGroupTotals(t *testing.T) {
	c := RankMembers([]SharedProgress{
		member(func(p *SharedProgress) { p.UserID, p.QuestionsAnswered, p.TopicsMastered = "a", 40, 2 }),
		member(func(p *SharedProgress) { p.UserID, p.QuestionsAnswered, p.TopicsMastered = "b", 60, 3 }),
	})
	if c.Totals != (Totals{QuestionsAnswered: 100, TopicsMastered: 5}) {
		t.Fatalf("totals = %+v", c.Totals)
	}
}

func TestTopStreak(t *testing.T) {
	streak := func(id, name string, days int) SharedProgress {
		return member(func(p *SharedProgress) { p.UserID, p.DisplayName, p.CurrentStreakDays = id, name, days })
	}
	c := RankMembers([]SharedProgress{streak("a", "Asha", 4), streak("b", "Bilal", 12)})
	if c.TopStreak == nil || *c.TopStreak != (TopStreak{DisplayName: "Bilal", Days: 12}) {
		t.Errorf("topStreak = %+v", c.TopStreak)
	}
	// Naming somebody as the streak leader on zero days would be a lie dressed as encouragement.
	if c := RankMembers([]SharedProgress{streak("a", "Learner", 0), streak("b", "Learner", 0)}); c.TopStreak != nil {
		t.Errorf("topStreak = %+v, want nil", c.TopStreak)
	}
}

func TestEmptyGroup(t *testing.T) {
	c := RankMembers([]SharedProgress{})
	if c.Members == nil || len(c.Members) != 0 || c.MemberCount != 0 || c.YourRank != nil || c.TopStreak != nil || c.Totals.QuestionsAnswered != 0 {
		t.Fatalf("got %+v", c)
	}
}

// The column CHECK is ^[A-HJ-NP-Z2-9]{6}$; a generator producing anything else would fail
// the insert. No O/0, I/1 or L either.
func TestInviteCodeShape(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		return b
	}
	valid := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{6}$`)
	ambiguous := regexp.MustCompile(`[O0I1L]`)
	for range 100 {
		code := MakeInviteCode(random)
		if len(code) != InviteCodeLength || !valid.MatchString(code) || ambiguous.MatchString(code) {
			t.Fatalf("bad code %q", code)
		}
	}
}

func TestInviteCodeDeterministic(t *testing.T) {
	// Bytes 0,1,2,... so the mapping is checkable rather than merely plausible.
	sequential := func() func(int) []byte {
		next := 0
		return func(n int) []byte {
			b := make([]byte, n)
			for i := range b {
				b[i] = byte(next)
				next++
			}
			return b
		}
	}
	if a, b := MakeInviteCode(sequential()), MakeInviteCode(sequential()); a != b {
		t.Fatalf("%q != %q", a, b)
	}
}

func TestInviteCodeRejectsBiasedBytes(t *testing.T) {
	// 248 and above would skew a 31-letter alphabet if taken modulo. Rejecting every byte on
	// the first passes forces the loop to ask again instead of hanging or emitting nothing.
	calls := 0
	code := MakeInviteCode(func(n int) []byte {
		calls++
		v := byte(250)
		if calls > 3 {
			v = 5
		}
		return slices.Repeat([]byte{v}, n)
	})
	if len(code) != InviteCodeLength || calls <= 1 {
		t.Fatalf("code %q after %d calls", code, calls)
	}
}

func TestNormaliseInviteCode(t *testing.T) {
	for in, want := range map[string]string{"  a b7k2m9 ": "AB7K2M9", "xk4p2q": "XK4P2Q"} {
		if got := NormaliseInviteCode(in); got != want {
			t.Errorf("NormaliseInviteCode(%q) = %q, want %q", in, got, want)
		}
	}
}
