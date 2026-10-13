package content

// OCR of scanned PDFs, with a fake model (no database, no API), plus one live run.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// scannedPDF is a PDF whose pages are the JPEG in testdata (rendered text, no text layer), except
// the pages listed in text, which carry that string as real text instead.
func scannedPDF(t *testing.T, pages int, text map[int]string) []byte {
	t.Helper()
	jpg, err := os.ReadFile("testdata/scanned-page.jpg")
	must(t, err)
	var b bytes.Buffer
	offsets := map[int]int{}
	obj := func(n int, body string) {
		offsets[n] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	b.WriteString("%PDF-1.4\n")
	kids := make([]string, pages)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 5+2*i)
	}
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	obj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	obj(4, fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 440 /Height 130 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", len(jpg), jpg))
	for i := range pages {
		content := "q 440 0 0 130 0 0 cm /Im1 Do Q"
		if s, ok := text[i+1]; ok {
			content = "BT /F1 12 Tf 10 60 Td (" + s + ") Tj ET"
		}
		obj(5+2*i, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 440 130] /Resources << /Font << /F1 3 0 R >> /XObject << /Im1 4 0 R >> >> /Contents %d 0 R >>", 6+2*i))
		obj(6+2*i, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	n := 5 + 2*pages
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", n)
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n, xref)
	return b.Bytes()
}

// fakeOCR answers OCR requests with "Text of page N." for every page asked for (plus a page that
// was not asked for, which must be ignored) and figure requests with none. It records the
// page lists it was asked for.
func fakeOCR(fail error) (*ai.Client, *[]string) {
	var mu sync.Mutex
	asked := []string{}
	return ai.NewFake(func(r ai.Request) (string, error) {
		if r.System != ai.OCRSystem {
			return `{"figures": []}`, nil
		}
		if len(r.Attachments) != 1 || !r.Attachments[0].PDF || r.Quality != ai.Standard {
			return "", errors.New("OCR must send the PDF at Standard quality")
		}
		list := strings.TrimSuffix(strings.TrimPrefix(r.Prompt, "Transcribe the text of these pages of the attached PDF: "), ".")
		mu.Lock()
		asked = append(asked, list)
		mu.Unlock()
		if fail != nil {
			return "", fail
		}
		var pages []string
		for _, n := range strings.Split(list, ", ") {
			pages = append(pages, fmt.Sprintf(`{"page": %s, "text": "Text of page %s, read from the scan."}`, n, n))
		}
		pages = append(pages, `{"page": 999, "text": "not asked for"}`)
		return `{"pages": [` + strings.Join(pages, ",") + `]}`, nil
	}), &asked
}

func ocrSvc(client *ai.Client) *svc { return &svc{d: &app.Deps{AI: client}} }

func pagesOf(chunks []store.NewChunk) map[int]string {
	out := map[int]string{}
	for _, c := range chunks {
		if c.Modality == "text" {
			out[*c.Page] += c.Content
		}
	}
	return out
}

func TestScannedPDFIsReadWithOCRAndKeepsPageNumbers(t *testing.T) {
	client, asked := fakeOCR(nil)
	chunks, err := ocrSvc(client).ingestPDF(context.Background(), scannedPDF(t, 1, nil), false)
	must(t, err)
	if got := pagesOf(chunks); len(got) != 1 || got[1] != "Text of page 1, read from the scan." {
		t.Fatalf("pages %v", got)
	}
	if !slices.Equal(*asked, []string{"1"}) {
		t.Fatalf("asked %v", *asked)
	}
}

func TestOnlyPagesWithoutTextAreSentForOCR(t *testing.T) {
	client, asked := fakeOCR(nil)
	file := scannedPDF(t, 3, map[int]string{1: "Normalisation removes redundancy from relational tables."})
	chunks, err := ocrSvc(client).ingestPDF(context.Background(), file, false)
	must(t, err)
	got := pagesOf(chunks)
	if !strings.Contains(got[1], "Normalisation removes redundancy") || got[2] != "Text of page 2, read from the scan." || got[3] == "" || len(got) != 3 {
		t.Fatalf("pages %v", got)
	}
	if !slices.Equal(*asked, []string{"2, 3"}) {
		t.Fatalf("asked %v", *asked)
	}
}

func TestLargeScansAreReadInPageRanges(t *testing.T) {
	client, asked := fakeOCR(nil)
	chunks, err := ocrSvc(client).ingestPDF(context.Background(), scannedPDF(t, 20, nil), false)
	must(t, err)
	if got := pagesOf(chunks); len(got) != 20 || got[20] != "Text of page 20, read from the scan." {
		t.Fatalf("pages %v", got)
	}
	slices.Sort(*asked)
	if want := []string{"1, 2, 3, 4, 5, 6, 7, 8", "17, 18, 19, 20", "9, 10, 11, 12, 13, 14, 15, 16"}; !slices.Equal(*asked, want) {
		t.Fatalf("asked %v", *asked)
	}
}

func TestScannedPDFThatCannotBeReadFailsWithAReason(t *testing.T) {
	ctx := context.Background()
	isFailure := func(err error, want string) {
		t.Helper()
		var f failure
		if !errors.As(err, &f) || !strings.Contains(f.msg, want) {
			t.Fatalf("want a failure containing %q, got %v", want, err)
		}
	}
	broken, _ := fakeOCR(errors.New("model overloaded"))
	_, err := ocrSvc(broken).ingestPDF(ctx, scannedPDF(t, 1, nil), false)
	isFailure(err, "reading it with OCR failed (model overloaded)")

	_, err = ocrSvc(nil).ingestPDF(ctx, scannedPDF(t, 1, nil), false)
	isFailure(err, "no AI provider configured")

	client, asked := fakeOCR(nil)
	_, err = ocrSvc(client).ingestPDF(ctx, scannedPDF(t, maxPDFPagesForFigures+1, nil), false)
	isFailure(err, "read with OCR up to 100 pages")
	if len(*asked) != 0 {
		t.Fatalf("a scan over the limit was sent: %v", *asked)
	}

	// Some real text: OCR failing costs only the scanned pages.
	chunks, err := ocrSvc(broken).ingestPDF(ctx, scannedPDF(t, 2, map[int]string{1: "Normalisation removes redundancy from relational tables."}), false)
	must(t, err)
	if got := pagesOf(chunks); len(got) != 1 || got[1] == "" {
		t.Fatalf("pages %v", got)
	}
}

// Reads the testdata scan with the real model. GEMINI_LIVE=1 (free tier) or AI_LIVE=1 (Claude).
func TestScannedPDFLive(t *testing.T) {
	settings := ai.Settings{Provider: "gemini", GeminiKey: getenv("GEMINI_API_KEY")}
	switch {
	case os.Getenv("AI_LIVE") != "":
		settings = ai.Settings{Provider: "claude", AnthropicKey: getenv("ANTHROPIC_API_KEY")}
	case os.Getenv("GEMINI_LIVE") == "":
		t.Skip("set GEMINI_LIVE=1 or AI_LIVE=1 to call the real API")
	}
	client := ai.New(settings)
	if client == nil {
		t.Skip("no API key for " + settings.Provider)
	}
	chunks, err := ocrSvc(client).ingestPDF(context.Background(), scannedPDF(t, 1, nil), false)
	must(t, err)
	text := strings.ToLower(pagesOf(chunks)[1])
	t.Logf("%s read page 1 as: %q", settings.Provider, text)
	for _, want := range []string{"hashing", "hash table", "bucket", "load factor"} {
		if !strings.Contains(text, want) {
			t.Errorf("OCR text lacks %q", want)
		}
	}
}
