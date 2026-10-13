package content

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// Course-map cases: parsing, topic assignment and excerpt locations.

func decode(t *testing.T, s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseCourseMapDropsDuplicatesAndEmptySubjectsAndClampsWeights(t *testing.T) {
	m, err := parseCourseMap(decode(t, `{"subjects": [
		{"name": "Sorting", "weight": 9, "topics": [
			{"name": "Merge sort", "keyConcepts": ["divide"], "prerequisites": [], "excerptIds": [0, 1]},
			{"name": "Merge sort", "keyConcepts": [], "prerequisites": [], "excerptIds": [2]}
		]},
		{"name": "Empty", "weight": 1, "topics": []}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m[0].Weight != 5 || len(m[0].Topics) != 1 {
		t.Fatalf("got %+v", m)
	}
}

func TestParseCourseMapRefusesAnEmptyMap(t *testing.T) {
	if _, err := parseCourseMap(decode(t, `{"subjects": []}`)); err == nil {
		t.Fatal("want an error")
	}
}

func TestAssignTopicsUsesNearestSeenNeighbourInSameSource(t *testing.T) {
	chunks := []store.ChunkRow{{ID: "a", SourceID: "s1"}, {ID: "b", SourceID: "s1"}, {ID: "c", SourceID: "s1"}, {ID: "d", SourceID: "s2"}}
	got := assignTopics(chunks, map[int]string{0: "T1", 3: "T2"})
	want := []store.ChunkAssignment{{ChunkID: "a", TopicID: "T1"}, {ChunkID: "b", TopicID: "T1"}, {ChunkID: "c", TopicID: "T1"}, {ChunkID: "d", TopicID: "T2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestLocationOf(t *testing.T) {
	for _, tc := range []struct {
		c    store.ChunkRow
		want string
	}{
		{store.ChunkRow{Page: ptr(42)}, "p. 42"},
		{store.ChunkRow{Slide: ptr(7)}, "slide 7"},
		{store.ChunkRow{StartSec: ptr(725)}, "12:05"},
		{store.ChunkRow{StartSec: ptr(3725)}, "1:02:05"},
	} {
		if got := locationOf(tc.c); got != tc.want {
			t.Errorf("locationOf(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestFileFormat(t *testing.T) {
	if fileFormat([]byte("%PDF-1.4")) != "pdf" || fileFormat([]byte("PK\x03\x04rest")) != "pptx" || fileFormat([]byte("hello")) != "" {
		t.Fatal("magic bytes misread")
	}
}
