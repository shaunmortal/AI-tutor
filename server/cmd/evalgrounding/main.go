// Command evalgrounding evaluates the source-grounded tutor on a team-built test set, end to end.
//
// Runs exactly what the tutor runs (ai.AnswerFromMaterial over the tutor's own retrieval: the
// grounded prompt and the citation checks) against real course material -- MIT 6.006
// Introduction to Algorithms, lectures 1-3: the lecture-notes PDFs and the lecture videos -- and
// measures:
//
//	retrieval hit rate     an excerpt at an expected location was among those retrieved
//	citation accuracy      the answer cited an excerpt at an expected location
//	citation precision     share of cited excerpts that are at an expected location
//	false refusal rate     in-material questions answered "not covered"
//	correct refusal rate   off-material questions answered "not covered"
//
// and writes one JSON line per question with the retrieved contexts, so eval/ragas_eval.py can
// score faithfulness, answer relevancy and context precision/recall with a standard framework.
//
// Usage, from server/ (reads server/.env):
//
//	go run ./cmd/evalgrounding -user <learner uuid> [-ingest]
//	go run ./cmd/evalgrounding -dry-run [-user <learner uuid>]
//
// -ingest adds the six sources for that learner first and reads them in this process; titles
// must match the test set's "source" names, which is why the command does it rather than a person.
//
// -dry-run checks the test set and the database, and runs retrieval alone (no Claude, no cost).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/config"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/eval"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
	"github.com/shaunmortal/AI-tutor/server/internal/web/content"
)

const ocw = "https://ocw.mit.edu/courses/6-006-introduction-to-algorithms-spring-2020"

// course is the material, with the exact titles the test set refers to. One of pdf or video.
var course = []struct{ title, pdf, video string }{
	{title: "Lecture 1 notes", pdf: ocw + "/477c78e0af2df61fa205bcc6cb613ceb_MIT6_006S20_lec1.pdf"},
	{title: "Lecture 2 notes", pdf: ocw + "/79a07dc1cb47d76dae2ffedc701e3d2b_MIT6_006S20_lec2.pdf"},
	{title: "Lecture 3 notes", pdf: ocw + "/6d1ae5278d02bbecb5c4428928b24194_MIT6_006S20_lec3.pdf"},
	{title: "Lecture 1 video", video: "https://www.youtube.com/watch?v=ZA-tUyM_y7s"},
	{title: "Lecture 2 video", video: "https://www.youtube.com/watch?v=CHhwJjR0mZA"},
	{title: "Lecture 3 video", video: "https://www.youtube.com/watch?v=oS9aPzUNG-s"},
}

type testItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"` // in_material, off_material or figure
	Question  string          `json:"question"`
	Reference string          `json:"reference"`
	Expected  []eval.Expected `json:"expected"`
}

func main() {
	user := flag.String("user", "", "learner uuid whose course material is searched")
	ingest := flag.Bool("ingest", false, "add and read the six course sources for -user first")
	dryRun := flag.Bool("dry-run", false, "check the test set and database and run retrieval only; no Claude calls, no output files")
	testset := flag.String("testset", "../eval/testset.json", "test set to run")
	out := flag.String("out", "../eval/results", "directory for grounding.jsonl and grounding-summary.json")
	flag.Parse()
	if err := run(*user, *ingest, *dryRun, *testset, *out); err != nil {
		log.Fatal(err)
	}
}

func run(userID string, ingest, dryRun bool, testsetPath, outDir string) error {
	items, err := readTestset(testsetPath)
	if err != nil {
		return err
	}
	fmt.Printf("test set: %d questions, valid\n", len(items))
	if userID == "" && !dryRun {
		return errors.New("pass -user <learner uuid>")
	}

	config.LoadDotenv(".env")
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	database, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBTransactionPooler)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Admin.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	fmt.Println("database: reachable")
	if userID == "" {
		return nil
	}

	client := ai.New(cfg.AnthropicAPIKey)
	if client == nil && (ingest || !dryRun) {
		return errors.New("ANTHROPIC_API_KEY is not set")
	}
	sources := store.NewSources(database.Admin)
	if ingest && !dryRun {
		if err := ingestCourse(ctx, &app.Deps{Cfg: cfg, DB: database, AI: client}, sources, userID); err != nil {
			return err
		}
	}

	// Citations carry location labels ("p. 3", "12:05"); matching ranges needs the raw numbers.
	chunks, err := sources.ListChunks(ctx, userID)
	if err != nil {
		return err
	}
	byID := map[string]store.ChunkRow{}
	for _, c := range chunks {
		byID[c.ID] = c
	}
	locate := func(c ai.Citation) eval.Located {
		row := byID[c.ChunkID]
		page := row.Page
		if page == nil {
			page = row.Slide
		}
		return eval.Located{SourceTitle: c.SourceTitle, Page: page, StartSec: row.StartSec, EndSec: row.EndSec}
	}
	labels := func(cs []ai.Citation) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.SourceTitle + ", " + c.Location
		}
		return out
	}
	countHits := func(cs []ai.Citation, expected []eval.Expected) int {
		n := 0
		for _, c := range cs {
			if eval.Matches(locate(c), expected) {
				n++
			}
		}
		return n
	}

	material := content.NewMaterial(sources)
	if dryRun {
		fmt.Printf("learner %s: %d excerpts\n", userID, len(chunks))
		if err := checkSources(ctx, sources, userID, items); err != nil {
			return err
		}
		hits, in := 0, 0
		for _, item := range items {
			// The same query and limit AnswerFromMaterial uses: the question, six excerpts.
			retrieved, err := material.Retrieve(ctx, userID, item.Question, 6)
			if err != nil {
				return err
			}
			hit := "-"
			if item.Type != "off_material" {
				in++
				hit = "miss"
				if countHits(retrieved, item.Expected) > 0 {
					hits++
					hit = "hit"
				}
			}
			fmt.Printf("%s %s retrieved=%d %s\n", item.ID, item.Type, len(retrieved), hit)
		}
		fmt.Printf("dry run: retrieval hit rate %d/%d in-material; Claude not called, nothing written\n", hits, in)
		return nil
	}

	var rows []eval.Row
	for _, item := range items {
		result, err := ai.AnswerFromMaterial(ctx, client, material, userID, item.Question, ai.TutorContext{}, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", item.ID, err)
		}
		inMaterial := item.Type != "off_material"
		row := eval.Row{
			ID: item.ID, Type: item.Type, Question: item.Question, Reference: item.Reference,
			Answer: result.Text, Grounding: string(*result.Grounding),
			Contexts:  []string{},
			Retrieved: labels(result.Retrieved),
			Cited:     labels(result.Citations),
			Expected:  item.Expected,
			Refused:   *result.Grounding == ai.GroundingNone,
		}
		for _, c := range result.Retrieved {
			row.Contexts = append(row.Contexts, c.Excerpt)
		}
		if inMaterial {
			retrievalHit := countHits(result.Retrieved, item.Expected) > 0
			citedHits := countHits(result.Citations, item.Expected)
			citationHit := citedHits > 0
			row.RetrievalHit, row.CitationHit = &retrievalHit, &citationHit
			if len(result.Citations) > 0 {
				precision := float64(citedHits) / float64(len(result.Citations))
				row.CitationPrecision = &precision
			}
		}
		rows = append(rows, row)
		fmt.Printf("%s %s grounding=%s cited=%d\n", item.ID, item.Type, row.Grounding, len(result.Citations))
	}

	summary := eval.Summarise(rows)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	var lines bytes.Buffer
	enc := json.NewEncoder(&lines) // one JSON value per line
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "grounding.jsonl"), lines.Bytes(), 0o644); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "grounding-summary.json"), data, 0o644); err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func readTestset(path string) ([]testItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []testItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, item := range items {
		bad := func(msg string) error { return fmt.Errorf("%s: item %d (%q): %s", path, i, item.ID, msg) }
		switch {
		case item.ID == "" || seen[item.ID]:
			return nil, bad("missing or duplicate id")
		case !slices.Contains([]string{"in_material", "off_material", "figure"}, item.Type):
			return nil, bad("type must be in_material, off_material or figure")
		case item.Question == "":
			return nil, bad("empty question")
		case item.Type != "off_material" && (item.Reference == "" || len(item.Expected) == 0):
			return nil, bad("an in-material question needs a reference and expected locations")
		}
		for _, e := range item.Expected {
			if e.Source == "" || (e.Page == nil && e.Slide == nil && e.StartSec == nil) {
				return nil, bad("each expected location needs a source and a page, slide or startSec")
			}
		}
		seen[item.ID] = true
	}
	return items, nil
}

// checkSources lists the learner's sources and warns about any the test set expects that is not ready.
func checkSources(ctx context.Context, sources *store.Sources, userID string, items []testItem) error {
	list, err := sources.List(ctx, userID)
	if err != nil {
		return err
	}
	ready := map[string]bool{}
	for _, s := range list {
		fmt.Printf("  %s: %s (%d excerpts)\n", s.Title, s.Status, s.ChunkCount)
		ready[s.Title] = ready[s.Title] || s.Status == "ready"
	}
	var missing []string
	for _, item := range items {
		for _, e := range item.Expected {
			if !ready[e.Source] && !slices.Contains(missing, e.Source) {
				missing = append(missing, e.Source)
			}
		}
	}
	if len(missing) > 0 {
		fmt.Printf("WARNING: no ready source titled %q; retrieval cannot hit them (run with -ingest)\n", missing)
	}
	return nil
}

func ingestCourse(ctx context.Context, d *app.Deps, sources *store.Sources, userID string) error {
	for _, item := range course {
		in := store.NewSource{UserID: userID, Title: item.title, Kind: "pdf"}
		var file []byte
		if item.pdf != "" {
			res, err := http.Get(item.pdf)
			if err != nil {
				return err
			}
			file, err = io.ReadAll(res.Body)
			res.Body.Close()
			if err == nil && res.StatusCode != http.StatusOK {
				err = fmt.Errorf("%s: HTTP %d", item.pdf, res.StatusCode)
			}
			if err != nil {
				return err
			}
		} else {
			in.Kind, in.VideoURL = "video", &item.video
		}
		if err := content.IngestNow(ctx, d, in, file); err != nil {
			fmt.Printf("%s: %v\n", item.title, err)
		}
	}
	list, err := sources.List(ctx, userID)
	if err != nil {
		return err
	}
	for _, s := range list {
		msg := ""
		if s.Error != nil {
			msg = " " + *s.Error
		}
		fmt.Printf("%s: %s (%d excerpts)%s\n", s.Title, s.Status, s.ChunkCount, msg)
	}
	return nil
}
