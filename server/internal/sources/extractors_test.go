package sources

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseCaptions(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:04.500\nHello <b>class</b>\n\n01:02:03.250 --> 01:02:05.000\nLater\n"
	srt := "1\n00:00:10,000 --> 00:00:12,000\nFirst line\nsecond line\n\n2\n00:00:12,000 --> 00:00:14,000\nNext\n"

	if got, want := ParseCaptions(vtt), []TranscriptSegment{{1, 4, "Hello class"}, {3723, 3725, "Later"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("vtt = %+v, want %+v", got, want)
	}
	if got, want := ParseCaptions(srt)[0], (TranscriptSegment{10, 12, "First line second line"}); got != want {
		t.Errorf("srt[0] = %+v, want %+v", got, want)
	}
}

func TestWindowTranscript(t *testing.T) {
	got := WindowTranscript([]TranscriptSegment{{0, 5, "a"}, {30, 35, "b"}, {61, 66, "c"}}, 60)
	want := []TranscriptSegment{{0, 35, "a b"}, {61, 66, "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSplitText(t *testing.T) {
	if got := SplitText("short", MaxChunkChars); !reflect.DeepEqual(got, []string{"short"}) {
		t.Errorf("short text = %q", got)
	}
	for name, tc := range map[string]struct {
		text      string
		wantMulti bool
	}{
		"sentences never exceed the limit":  {strings.Repeat("One sentence here. ", 400), true},
		"a single unbroken run is cut hard": {strings.Repeat("x", 1200), false},
	} {
		pieces := SplitText(tc.text, 500)
		if tc.wantMulti && len(pieces) <= 1 {
			t.Errorf("%s: got %d pieces", name, len(pieces))
		}
		for _, p := range pieces {
			if len([]rune(p)) > 500 {
				t.Errorf("%s: piece of %d chars", name, len([]rune(p)))
			}
		}
	}
}

func TestYoutubeLinks(t *testing.T) {
	for _, url := range []string{
		"https://www.youtube.com/watch?v=ZA-tUyM_y7s",
		"https://youtu.be/ZA-tUyM_y7s",
		"https://www.youtube.com/embed/ZA-tUyM_y7s",
	} {
		if got := YoutubeID(url); got != "ZA-tUyM_y7s" {
			t.Errorf("YoutubeID(%q) = %q", url, got)
		}
	}
	if got := YoutubeAt("https://youtu.be/ZA-tUyM_y7s", 125); got != "https://www.youtube.com/watch?v=ZA-tUyM_y7s&t=125s" {
		t.Errorf("YoutubeAt = %q", got)
	}
	if got := YoutubeID("https://example.com/video"); got != "" {
		t.Errorf("non-YouTube id = %q", got)
	}
}

// minimalPDF builds a PDF whose pages carry the given content streams.
func minimalPDF(streams ...string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	n := len(streams)
	kids := make([]string, n)
	for i := range streams {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, s := range streams {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s), s))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

func TestExtractPDFPages(t *testing.T) {
	file := minimalPDF(
		"BT ET",
		// No space characters: the word gap and the line break come from positions alone.
		"BT /F1 12 Tf 20 250 Td [(Hello)-600(world)] TJ 0 -20 Td (Next line) Tj ET",
	)
	got, err := ExtractPDFPages(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := []PageText{{2, "Hello world\nNext line"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestExtractPPTXSlides(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"ppt/slides/slide10.xml":            `<p:sld><a:p><a:r><a:t>   </a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":             `<p:sld><a:p><a:r><a:t>Title &amp; more</a:t></a:r></a:p><a:p><a:r><a:t>Point</a:t></a:r><a:r><a:t> one</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/_rels/slide2.xml.rels":  `<Relationships><Relationship Target="../notesSlides/notesSlide1.xml"/><Relationship Target="../media/image1.png"/><Relationship Target="../media/gone.jpg"/><Relationship Target="../slideLayouts/slideLayout1.xml"/></Relationships>`,
		"ppt/notesSlides/notesSlide1.xml":   `<p:notes><a:p><a:r><a:t>Say hi</a:t></a:r></a:p></p:notes>`,
		"ppt/media/image1.png":              "PNG",
		"ppt/slideLayouts/slideLayout1.xml": `<a:p><a:t>layout</a:t></a:p>`,
	} {
		w, _ := zw.Create(name)
		io.WriteString(w, body)
	}
	zw.Close()

	got, err := ExtractPPTXSlides(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []SlideContent{
		{Slide: 2, Text: "Title & more\nPoint one\nSpeaker notes: Say hi", Images: []Image{{"image/png", base64.StdEncoding.EncodeToString([]byte("PNG"))}}},
		{Slide: 10, Text: "", Images: []Image{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchYoutubeSegmentsOffline(t *testing.T) {
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		var body string
		switch {
		case r.URL.Path == "/watch":
			body = `<script>ytcfg.set({"INNERTUBE_API_KEY":"KEY123"})</script>`
		case r.URL.Path == "/youtubei/v1/player" && r.URL.Query().Get("key") == "KEY123":
			body = `{"playabilityStatus":{"status":"OK"},"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[
				{"languageCode":"fr","baseUrl":"https://www.youtube.com/api/timedtext?lang=fr"},
				{"languageCode":"en","baseUrl":"https://www.youtube.com/api/timedtext?v=x&fmt=srv3&lang=en"}]}}}`
		case r.URL.Path == "/api/timedtext" && r.URL.RawQuery == "v=x&lang=en":
			body = `<transcript><text start="1.5" dur="2.2">it&amp;#39;s a heap</text><text start="4" dur="1">  </text><text start="5.25" dur="0.5">a &lt; b</text></transcript>`
		default:
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	got, err := FetchYoutubeSegments(context.Background(), hc, "https://youtu.be/ZA-tUyM_y7s")
	if err != nil {
		t.Fatal(err)
	}
	want := []TranscriptSegment{{1, 4, "it's a heap"}, {5, 6, "a < b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Fetches a real MIT 6.006 lecture's captions. Run with YOUTUBE_LIVE=1.
func TestFetchYoutubeSegmentsLive(t *testing.T) {
	if os.Getenv("YOUTUBE_LIVE") == "" {
		t.Skip("set YOUTUBE_LIVE=1 to fetch from YouTube")
	}
	segs, err := FetchYoutubeSegments(context.Background(), nil, "https://www.youtube.com/watch?v=ZA-tUyM_y7s")
	if err != nil {
		t.Fatal(err)
	}
	windows := WindowTranscript(segs, TranscriptWindowSec)
	t.Logf("%d segments, %d windows; first: %+v", len(segs), len(windows), windows[0])
}

// Regression: runs with attributes (xml:space="preserve") were skipped, and numeric entities
// were left as "&#8217;".
func TestExtractPPTXPreservedRunsAndNumericEntities(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ppt/slides/slide1.xml")
	io.WriteString(w, `<p:sld><a:p><a:r><a:t>It</a:t></a:r><a:r><a:t xml:space="preserve">&#8217;s </a:t></a:r><a:r><a:t>a &#x2014; test &amp; more</a:t></a:r></a:p></p:sld>`)
	zw.Close()
	got, err := ExtractPPTXSlides(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if want := "It’s a — test & more"; len(got) != 1 || got[0].Text != want {
		t.Fatalf("got %+v, want %q", got, want)
	}
}
