// Command simulate runs the offline personalization evaluation (see internal/sim) and writes
// eval/results/simulation-results.json. Run from server/: go run ./cmd/simulate
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaunmortal/AI-tutor/server/internal/sim"
)

func main() {
	out := flag.String("out", "../eval/results/simulation-results.json", "where to write the results")
	flag.Parse()

	result, err := sim.Run(sim.SimConfig.Seeds)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(sim.Table(result.Summaries))
	seeds := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(sim.SimConfig.Seeds)), ","), "[]")
	fmt.Println("\nGains and calibration error are in mastery points (0-100). Means over seeds", seeds)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		log.Fatal(err)
	}
	abs, _ := filepath.Abs(*out)
	fmt.Println("\nWrote", abs)
}
