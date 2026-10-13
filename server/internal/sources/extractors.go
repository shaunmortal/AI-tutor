// Package sources turns course material into located units of text.
//
// Everything here answers one question: what does this file say, and where exactly does it
// say it. The location is the point — a page, a slide, a second in a video — because the tutor
// cites it and the learner clicks it. Nothing here calls the model; figures are described later
// during ingestion, because that costs money and this does not.
package sources

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// PageText is the text of one PDF page, numbered from 1.
type PageText struct {
	Page int
	Text string
}

// Image is a picture embedded in a slide.
type Image struct {
	MediaType string // image/png, image/jpeg or image/gif
	Base64    string
}

// SlideContent is one slide's text (with speaker notes) and pictures.
type SlideContent struct {
	Slide  int
	Text   string
	Images []Image
}

// TranscriptSegment is a stretch of speech, in whole seconds.
type TranscriptSegment struct {
	StartSec int
	EndSec   int
	Text     string
}

const (
	// MaxChunkChars is the excerpt size: small enough to cite precisely, large enough to carry one idea.
	MaxChunkChars = 1500
	// TranscriptWindowSec is a lecture transcript window, about one spoken paragraph.
	TranscriptWindowSec = 60
)

// ExtractPDFPages returns the text of every page that has any.
//
// Words and lines are rebuilt from glyph positions: LaTeX output has no space characters,
// and the library's own text helpers run words together.
func ExtractPDFPages(file []byte) ([]PageText, error) {
	r, err := pdf.NewReader(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		return nil, err
	}
	var pages []PageText
	for i := 1; i <= r.NumPage(); i++ {
		// Some fonts map glyphs to U+0000, which Postgres text cannot store.
		if text := tidy(strings.ReplaceAll(pageText(r.Page(i)), "\x00", "")); text != "" {
			pages = append(pages, PageText{Page: i, Text: text})
		}
	}
	return pages, nil
}

// PDFPageCount is the number of pages, with or without text: a scanned page has none.
func PDFPageCount(file []byte) (int, error) {
	r, err := pdf.NewReader(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		return 0, err
	}
	return r.NumPage(), nil
}

// Tidy normalises text from elsewhere (OCR) the way extracted page text is.
func Tidy(text string) string { return tidy(strings.ReplaceAll(text, "\x00", "")) }

var ligatures = strings.NewReplacer("ﬀ", "ff", "ﬁ", "fi", "ﬂ", "fl", "ﬃ", "ffi", "ﬄ", "ffl", "ﬅ", "st", "ﬆ", "st")

// pageText lays out one page's glyphs: a new line when the baseline moves by more than half
// the font size, a space when the gap to the previous glyph is wider than 0.15 em. A page the
// library cannot parse reads as empty rather than failing the whole document.
func pageText(p pdf.Page) (text string) {
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	if p.V.IsNull() {
		return ""
	}
	var b strings.Builder
	var prev pdf.Text
	started := false
	for _, t := range p.Content().Text {
		// The library emits "\n" after every TJ (decoded through the font, so sometimes as
		// U+FFFD), and U+FFFD for glyphs it cannot map. Layout comes from coordinates instead.
		if t.S == "\n" || t.S == "\uFFFD" || t.S == "" {
			continue
		}
		if started {
			size := max(math.Abs(t.FontSize), math.Abs(prev.FontSize))
			switch {
			case math.Abs(t.Y-prev.Y) > size*0.5:
				b.WriteByte('\n')
			case t.X-(prev.X+prev.W) > size*0.15 && prev.S != " " && t.S != " ":
				b.WriteByte(' ')
			}
		}
		b.WriteString(t.S)
		prev, started = t, true
	}
	return ligatures.Replace(b.String())
}

var (
	slidePath = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	relTarget = regexp.MustCompile(`Target="([^"]+)"`)
	// Attributes allowed: PowerPoint writes <a:t xml:space="preserve"> for runs with edge spaces.
	textRun = regexp.MustCompile(`<a:t(?:\s[^>]*)?>([^<]*)</a:t>`)
)

// ExtractPPTXSlides returns slide text, speaker notes and pictures, in slide order.
//
// A .pptx is a zip of XML. Slide N lives at ppt/slides/slideN.xml, its pictures are named in
// ppt/slides/_rels/slideN.xml.rels, and its notes in a notesSlide the same rels file points to.
func ExtractPPTXSlides(file []byte) ([]SlideContent, error) {
	zr, err := zip.NewReader(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		return nil, err
	}
	parts := make(map[string]*zip.File, len(zr.File))
	type numbered struct {
		n    int
		path string
	}
	var paths []numbered
	for _, f := range zr.File {
		parts[f.Name] = f
		if m := slidePath.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			paths = append(paths, numbered{n, f.Name})
		}
	}
	slices.SortStableFunc(paths, func(a, b numbered) int { return a.n - b.n })

	read := func(name string) ([]byte, bool, error) {
		f, ok := parts[name]
		if !ok {
			return nil, false, nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil, true, err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return b, true, err
	}

	slides := make([]SlideContent, 0, len(paths))
	for _, p := range paths {
		xml, _, err := read(p.path)
		if err != nil {
			return nil, err
		}
		rels, hasRels, err := read(fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", p.n))
		if err != nil {
			return nil, err
		}
		var targets []string
		if hasRels {
			for _, m := range relTarget.FindAllStringSubmatch(string(rels), -1) {
				targets = append(targets, m[1])
			}
		}

		notes := ""
		if i := slices.IndexFunc(targets, func(t string) bool { return strings.Contains(t, "notesSlides/") }); i >= 0 {
			notesXML, ok, err := read(resolvePart(targets[i]))
			if err != nil {
				return nil, err
			}
			if ok {
				notes = textRuns(string(notesXML))
			}
		}

		images := []Image{}
		for _, t := range targets {
			mediaType := imageType(t)
			if mediaType == "" {
				continue
			}
			data, ok, err := read(resolvePart(t))
			if err != nil {
				return nil, err
			}
			if ok {
				images = append(images, Image{MediaType: mediaType, Base64: base64.StdEncoding.EncodeToString(data)})
			}
		}

		var lines []string
		if body := textRuns(string(xml)); body != "" {
			lines = append(lines, body)
		}
		if notes != "" {
			lines = append(lines, "Speaker notes: "+notes)
		}
		slides = append(slides, SlideContent{Slide: p.n, Text: tidy(strings.Join(lines, "\n")), Images: images})
	}
	return slides, nil
}

// resolvePart resolves a rels target, which is relative to ppt/slides/ ("../media/image3.png").
func resolvePart(target string) string {
	if rest, ok := strings.CutPrefix(target, "../"); ok {
		return "ppt/" + rest
	}
	return "ppt/slides/" + target
}

func imageType(path string) string {
	switch p := strings.ToLower(path); {
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(p, ".gif"):
		return "image/gif"
	}
	return ""
}

// textRuns is the visible text in DrawingML: every <a:t> run, one paragraph per <a:p>.
func textRuns(xml string) string {
	var lines []string
	for _, para := range strings.Split(xml, "</a:p>") {
		var line strings.Builder
		for _, m := range textRun.FindAllStringSubmatch(para, -1) {
			line.WriteString(decodeEntities(m[1]))
		}
		if strings.TrimFunc(line.String(), isJSSpace) != "" {
			lines = append(lines, line.String())
		}
	}
	return strings.Join(lines, "\n")
}

var cueHeader = regexp.MustCompile(`(\d{1,2}:)?(\d{1,2}):(\d{2})[.,](\d{3})\s*-->\s*(\d{1,2}:)?(\d{1,2}):(\d{2})[.,](\d{3})[^\n]*\n`)
var tag = regexp.MustCompile(`<[^>]+>`)

// ParseCaptions reads WebVTT or SRT captions, for a lecture whose captions came as a file.
// A cue's text runs from its timing line to the next blank line.
func ParseCaptions(captions string) []TranscriptSegment {
	s := strings.ReplaceAll(captions, "\r", "")
	var segments []TranscriptSegment
	for pos := 0; pos < len(s); {
		m := cueHeader.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return s[pos+m[2*i] : pos+m[2*i+1]]
		}
		start := seconds(group(1), group(2), group(3))
		end := seconds(group(5), group(6), group(7))
		bodyStart := pos + m[1]
		bodyEnd := bodyStart
		for bodyEnd < len(s) && !blankLineAt(s, bodyEnd) {
			bodyEnd++
		}
		text := strings.Join(strings.FieldsFunc(tag.ReplaceAllString(s[bodyStart:bodyEnd], ""), isJSSpace), " ")
		if text != "" {
			segments = append(segments, TranscriptSegment{StartSec: start, EndSec: end, Text: text})
		}
		pos = bodyEnd
	}
	return segments
}

// blankLineAt reports whether s[i:] starts with \n\s*\n: a newline, then whitespace that
// contains another newline.
func blankLineAt(s string, i int) bool {
	if s[i] != '\n' {
		return false
	}
	for _, r := range s[i+1:] {
		if r == '\n' {
			return true
		}
		if !isJSSpace(r) {
			return false
		}
	}
	return false
}

func seconds(hours, minutes, secs string) int {
	h := 0
	if hours != "" {
		h, _ = strconv.Atoi(strings.TrimSuffix(hours, ":"))
	}
	m, _ := strconv.Atoi(minutes)
	sec, _ := strconv.Atoi(secs)
	return h*3600 + m*60 + sec
}

// WindowTranscript joins caption segments into windows of windowSec: captions arrive a few
// words at a time, a citation needs a passage. It drops the repeated words auto-generated
// captions carry across cue boundaries.
func WindowTranscript(segments []TranscriptSegment, windowSec int) []TranscriptSegment {
	var windows []TranscriptSegment
	var current *TranscriptSegment
	for _, seg := range segments {
		if current == nil || seg.StartSec-current.StartSec >= windowSec {
			if current != nil {
				windows = append(windows, *current)
			}
			c := seg
			current = &c
			continue
		}
		current.EndSec = max(current.EndSec, seg.EndSec)
		if !strings.HasSuffix(current.Text, seg.Text) {
			current.Text += " " + seg.Text
		}
	}
	if current != nil {
		windows = append(windows, *current)
	}
	return windows
}

// SplitText splits long text at paragraph, then sentence, boundaries so no excerpt exceeds
// max characters. Lengths count runes.
func SplitText(text string, max int) []string {
	if len([]rune(text)) <= max {
		return []string{text}
	}
	var pieces []string
	var current []rune
	for _, sentence := range splitSentences(text) {
		s := []rune(sentence)
		if len(current) > 0 && len(current)+len(s)+1 > max {
			pieces = append(pieces, string(current))
			current = nil
		}
		// A single "sentence" longer than the limit (a table, a code listing) is cut hard.
		for start := 0; start < len(s); start += max {
			part := s[start:min(start+max, len(s))]
			if len(current) > 0 {
				current = append(append(current, ' '), part...)
			} else {
				current = append([]rune(nil), part...)
			}
			if len(current) >= max {
				pieces = append(pieces, string(current))
				current = nil
			}
		}
	}
	if strings.TrimFunc(string(current), isJSSpace) != "" {
		pieces = append(pieces, string(current))
	}
	out := pieces[:0]
	for _, p := range pieces {
		if p = strings.TrimFunc(p, isJSSpace); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitSentences is text.split(/(?<=[.!?])\s+|\n{2,}/): break after sentence punctuation
// followed by whitespace, or at a blank line.
func splitSentences(text string) []string {
	rs := []rune(text)
	var out []string
	from := 0
	for i := 0; i < len(rs); {
		end := i
		if i > 0 && strings.ContainsRune(".!?", rs[i-1]) && isJSSpace(rs[i]) {
			for end < len(rs) && isJSSpace(rs[end]) {
				end++
			}
		} else if rs[i] == '\n' && i+1 < len(rs) && rs[i+1] == '\n' {
			for end < len(rs) && rs[end] == '\n' {
				end++
			}
		}
		if end == i {
			i++
			continue
		}
		out = append(out, string(rs[from:i]))
		from, i = end, end
	}
	return append(out, string(rs[from:]))
}

var (
	youtubeIDInURL = regexp.MustCompile(`(?:youtu\.be/|v=|/embed/|/shorts/|/live/)([A-Za-z0-9_-]{11})`)
	bareYoutubeID  = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
)

// YoutubeID finds the video id in a YouTube address, or returns "".
func YoutubeID(url string) string {
	if m := youtubeIDInURL.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	if bareYoutubeID.MatchString(url) {
		return url
	}
	return ""
}

// YoutubeAt is a YouTube address that opens the lecture at a given second.
func YoutubeAt(url string, startSec int) string {
	if id := YoutubeID(url); id != "" {
		return "https://www.youtube.com/watch?v=" + id + "&t=" + strconv.Itoa(startSec) + "s"
	}
	return url
}

// --- YouTube captions, the way youtube-transcript-plus 2.0.3 fetches them ---

const (
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	captionLang   = "en"
	fetchRetries  = 2
	retryDelay    = time.Second
	maxPageBytes  = 16 << 20
	innertubeName = "ANDROID"
	innertubeVer  = "20.10.38"
)

var (
	looseYoutubeID = regexp.MustCompile(`(?i)(?:v=|/|v/|embed/|watch\?.*v=|youtu\.be/|/v/|e/|watch\?.*vi?=|/embed/|/v/|vi?/|watch\?.*vi?=|youtu\.be/|/vi?/|/e/)([a-zA-Z0-9_-]{11})`)
	apiKeyPlain    = regexp.MustCompile(`"INNERTUBE_API_KEY":"([^"]+)"`)
	apiKeyEscaped  = regexp.MustCompile(`INNERTUBE_API_KEY\\":\\"([^\\"]+)\\"`)
	fmtParam       = regexp.MustCompile(`&fmt=[^&]+`)
	xmlTranscript  = regexp.MustCompile(`<text start="([^"]*)" dur="([^"]*)">([^<]*)</text>`)
	xmlEntities    = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'")
)

// ErrNoCaptions: the video exists (as far as can be told) but has no usable captions, so
// transcribing it is worth a try. Test with errors.Is.
var ErrNoCaptions = errors.New("no captions")

type noCaptions struct{ error }

func (noCaptions) Is(target error) bool { return target == ErrNoCaptions }

// FetchYoutubeSegments returns timed captions for a YouTube lecture, from the captions
// YouTube already publishes: the watch page yields an Innertube key, the Innertube player
// (as the Android client) lists caption tracks, and the English track's XML is the transcript.
// hc may be nil.
func FetchYoutubeSegments(ctx context.Context, hc *http.Client, url string) ([]TranscriptSegment, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	id := YoutubeID(url)
	if id == "" {
		id = url
	}
	if !bareYoutubeID.MatchString(id) {
		m := looseYoutubeID.FindStringSubmatch(id)
		if m == nil {
			return nil, errors.New(`Invalid YouTube video ID or URL. Please provide a valid video ID or URL. Example: "dQw4w9WgXcQ" or "https://www.youtube.com/watch?v=dQw4w9WgXcQ".`)
		}
		id = m[1]
	}

	unavailable := fmt.Errorf(`The video with ID "%s" is no longer available or has been removed. Please check the video URL or ID and try again.`, id)
	notAvailable := noCaptions{fmt.Errorf(`No transcripts are available for the video with ID "%s". This may be because the video does not have captions or the captions are not accessible.`, id)}
	disabled := noCaptions{fmt.Errorf(`Transcripts are disabled for the video with ID "%s". This may be due to the video owner disabling captions or the video not supporting transcripts.`, id)}
	tooMany := errors.New("YouTube is receiving too many requests from your IP address. Please try again later or use a proxy. If the issue persists, consider reducing the frequency of requests.")

	status, page, err := fetch(ctx, hc, http.MethodGet, "https://www.youtube.com/watch?v="+id, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, unavailable
	}
	if bytes.Contains(page, []byte(`class="g-recaptcha"`)) {
		return nil, tooMany
	}
	key := apiKeyPlain.FindSubmatch(page)
	if key == nil {
		key = apiKeyEscaped.FindSubmatch(page)
	}
	if key == nil {
		return nil, notAvailable
	}

	body, _ := json.Marshal(map[string]any{
		"context": map[string]any{"client": map[string]any{"clientName": innertubeName, "clientVersion": innertubeVer}},
		"videoId": id,
	})
	status, raw, err := fetch(ctx, hc, http.MethodPost, "https://www.youtube.com/youtubei/v1/player?key="+string(key[1]), body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, unavailable
	}
	type tracklist struct {
		CaptionTracks json.RawMessage `json:"captionTracks"`
	}
	var player struct {
		Captions *struct {
			Renderer *tracklist `json:"playerCaptionsTracklistRenderer"`
		} `json:"captions"`
		Renderer          *tracklist `json:"playerCaptionsTracklistRenderer"`
		PlayabilityStatus struct {
			Status string `json:"status"`
		} `json:"playabilityStatus"`
	}
	if err := json.Unmarshal(raw, &player); err != nil {
		return nil, err
	}
	list := player.Renderer
	if player.Captions != nil && player.Captions.Renderer != nil {
		list = player.Captions.Renderer
	}
	if player.Captions == nil || list == nil {
		if player.PlayabilityStatus.Status == "OK" {
			return nil, disabled
		}
		return nil, notAvailable
	}
	type captionTrack struct {
		LanguageCode string `json:"languageCode"`
		BaseURL      string `json:"baseUrl"`
		URL          string `json:"url"`
	}
	var tracks []captionTrack
	if json.Unmarshal(list.CaptionTracks, &tracks) != nil || len(tracks) == 0 {
		return nil, disabled
	}
	i := slices.IndexFunc(tracks, func(t captionTrack) bool { return t.LanguageCode == captionLang })
	if i < 0 {
		var langs []string
		for _, t := range tracks {
			if t.LanguageCode != "" {
				langs = append(langs, t.LanguageCode)
			}
		}
		return nil, noCaptions{fmt.Errorf(`No transcripts are available in "%s" for the video with ID "%s". Available languages: %s. Please try a different language.`, captionLang, id, strings.Join(langs, ", "))}
	}
	transcriptURL := tracks[i].BaseURL
	if transcriptURL == "" {
		transcriptURL = tracks[i].URL
	}
	if transcriptURL == "" {
		return nil, notAvailable
	}
	// Prefer XML: drop the first fmt parameter.
	if loc := fmtParam.FindStringIndex(transcriptURL); loc != nil {
		transcriptURL = transcriptURL[:loc[0]] + transcriptURL[loc[1]:]
	}

	status, xml, err := fetch(ctx, hc, http.MethodGet, transcriptURL, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		if status == http.StatusTooManyRequests {
			return nil, tooMany
		}
		return nil, notAvailable
	}

	var segments []TranscriptSegment
	matches := xmlTranscript.FindAllSubmatch(xml, -1)
	if len(matches) == 0 {
		return nil, notAvailable
	}
	for _, m := range matches {
		start := parseFloat(string(m[1]))
		duration := parseFloat(string(m[2]))
		text := decodeEntities(xmlEntities.Replace(string(m[3])))
		if strings.TrimFunc(text, isJSSpace) == "" {
			continue
		}
		segments = append(segments, TranscriptSegment{
			StartSec: int(math.Floor(start)),
			EndSec:   int(math.Ceil(start + duration)),
			Text:     text,
		})
	}
	return segments, nil
}

// fetch makes one request, retrying 429 and 5xx with exponential backoff (1s, 2s).
func fetch(ctx context.Context, hc *http.Client, method, url string, body []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Language", captionLang)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := hc.Do(req)
		if err != nil {
			return 0, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes))
		res.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		retryable := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 && res.StatusCode <= 599
		if !retryable || attempt == fetchRetries {
			return res.StatusCode, data, nil
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(retryDelay << attempt):
		}
	}
}

// parseFloat reads a leading number the way JavaScript's parseFloat does for the values
// YouTube sends; anything unparseable is 0.
func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// decodeEntities decodes named and numeric (&#8217; &#x2019;) entities in one pass, plus the
// double-escaped apostrophe YouTube captions carry.
func decodeEntities(text string) string {
	return html.UnescapeString(strings.ReplaceAll(text, "&amp;#39;", "'"))
}

var (
	spacesTabs = regexp.MustCompile(`[ \t]+`)
	manyBreaks = regexp.MustCompile(`\n{3,}`)
)

func tidy(text string) string {
	text = spacesTabs.ReplaceAllString(text, " ")
	text = manyBreaks.ReplaceAllString(text, "\n\n")
	return strings.TrimFunc(text, isJSSpace)
}

// isJSSpace is JavaScript's \s: Unicode White_Space minus U+0085, plus U+FEFF.
func isJSSpace(r rune) bool {
	return r == '\uFEFF' || (r != '\u0085' && unicode.IsSpace(r))
}
