package sim

import (
	"reflect"
	"testing"
)

func TestDeterministicForFixedSeed(t *testing.T) {
	uneven := Profiles[1]
	for _, policy := range Policies {
		a, err := RunOne(uneven, policy, 7)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := RunOne(uneven, policy, 7)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: two runs with seed 7 differ", policy)
		}
	}
}

func TestAdaptiveWeakTopicGainAtLeastBaselineForUneven(t *testing.T) {
	res, err := Run(SimConfig.Seeds)
	if err != nil {
		t.Fatal(err)
	}
	gain := map[string]float64{}
	for _, s := range res.Summaries {
		if s.Profile == "uneven" {
			gain[s.Policy] = s.WeakTopicGain
		}
	}
	if gain["adaptive"] < gain["baseline"] {
		t.Errorf("adaptive weak-topic gain %v < baseline %v", gain["adaptive"], gain["baseline"])
	}
}

// The TypeScript mulberry32(1) stream, so the port cannot drift from the published results.
func TestMulberry32MatchesTypeScript(t *testing.T) {
	r := Mulberry32(1)
	for i, want := range []float64{0.6270739405881613, 0.002735721180215478, 0.5274470399599522} {
		if got := r(); got != want {
			t.Errorf("draw %d: got %v, want %v", i, got, want)
		}
	}
}
