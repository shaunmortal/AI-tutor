package ai

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func excerpt(n int) Citation {
	s := strconv.Itoa(n)
	return Citation{N: n, ChunkID: "chunk-" + s, SourceID: "source", SourceTitle: "Book", Kind: "pdf", Modality: "text", Location: "p. " + s, Excerpt: "excerpt " + s}
}

func citedNs(cs []Citation) []int {
	ns := []int{}
	for _, c := range cs {
		ns = append(ns, c.N)
	}
	return ns
}

func TestCheckGrounding(t *testing.T) {
	t.Run("keeps a cited, source-backed answer and returns only the excerpts it cites", func(t *testing.T) {
		text, g, cited := CheckGrounding(&GroundedReply{Grounding: "source", Answer: "A heap is a tree [2]. It is complete [2]."}, []Citation{excerpt(1), excerpt(2)})
		if g != GroundingSource || !slices.Equal(citedNs(cited), []int{2}) || !strings.Contains(text, "[2]") {
			t.Fatalf("got %q %q %v", text, g, citedNs(cited))
		}
	})

	t.Run("removes citation numbers that point at excerpts the model was never given", func(t *testing.T) {
		text, _, cited := CheckGrounding(&GroundedReply{Grounding: "source", Answer: "True [1]. Also true [9]."}, []Citation{excerpt(1)})
		if strings.Contains(text, "[9]") || !slices.Equal(citedNs(cited), []int{1}) {
			t.Fatalf("got %q %v", text, citedNs(cited))
		}
	})

	t.Run("does not let an uncited answer pass as coming from the course", func(t *testing.T) {
		text, g, cited := CheckGrounding(&GroundedReply{Grounding: "source", Answer: "Quicksort is O(n log n)."}, []Citation{excerpt(1)})
		if g != GroundingNone || len(cited) != 0 ||
			!strings.Contains(text, "not covered in your course material") ||
			!strings.Contains(text, "General knowledge, not from your course material") {
			t.Fatalf("got %q %q %v", text, g, cited)
		}
	})

	t.Run("declines clearly when the material does not cover the question", func(t *testing.T) {
		text, g, _ := CheckGrounding(&GroundedReply{Grounding: "none", Answer: "ignored", OutsideKnowledge: "This is about cooking."}, []Citation{excerpt(1)})
		if g != GroundingNone || !strings.HasPrefix(text, "This is not covered in your course material.") || strings.Contains(text, "ignored") {
			t.Fatalf("got %q %q", text, g)
		}
	})

	t.Run("keeps partial answers and labels the added knowledge separately", func(t *testing.T) {
		text, g, _ := CheckGrounding(&GroundedReply{Grounding: "partial", Answer: "Defined here [1].", OutsideKnowledge: "Used in Dijkstra."}, []Citation{excerpt(1)})
		want := regexp.MustCompile(`Defined here \[1\]\.\n\nGeneral knowledge, not from your course material: Used in Dijkstra\.`)
		if g != GroundingPartial || !want.MatchString(text) {
			t.Fatalf("got %q %q", text, g)
		}
	})

	t.Run("treats a malformed reply as not grounded", func(t *testing.T) {
		if _, g, _ := CheckGrounding(nil, []Citation{excerpt(1)}); g != GroundingNone {
			t.Fatalf("got %q", g)
		}
	})
}

func TestTitleFrom(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  what is\n a heap? ", "what is a heap?"},
		{strings.Repeat("word ", 20), "word word word word word word word word word word word word…"},
		{strings.Repeat("x", 70), strings.Repeat("x", 60) + "…"},
	} {
		if got := TitleFrom(tc.in); got != tc.want {
			t.Errorf("TitleFrom(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Regression: a "none" reply used to keep the excerpts it cited while blanking the answer, so
// the chips claimed a course source for text that said the course does not cover it.
func TestCheckGroundingNoneHasNoCitations(t *testing.T) {
	_, g, cited := CheckGrounding(&GroundedReply{Grounding: "none", Answer: "ignored [1]", OutsideKnowledge: "Cooking."}, []Citation{excerpt(1)})
	if g != GroundingNone || cited == nil || len(cited) != 0 {
		t.Fatalf("got %q %v", g, cited)
	}
}
