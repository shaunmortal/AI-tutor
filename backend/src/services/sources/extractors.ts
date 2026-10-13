import JSZip from 'jszip';
import { extractText, getDocumentProxy } from 'unpdf';
import { fetchTranscript, type TranscriptSegment as RawSegment } from 'youtube-transcript-plus';

/**
 * Turning a file into located units of text.
 *
 * Everything here answers one question: what does this file say, and where exactly does it say
 * it. The location is the point — a page, a slide, a second in a video — because the tutor cites
 * it and the learner clicks it. Nothing here calls the model; figures are described later, in the
 * ingestion service, because that costs money and this does not.
 */

export interface PageText {
    page: number;
    text: string;
}

export interface SlideContent {
    slide: number;
    text: string;
    images: { mediaType: 'image/png' | 'image/jpeg' | 'image/gif'; base64: string }[];
}

export interface TranscriptSegment {
    startSec: number;
    endSec: number;
    text: string;
}

/** Excerpt size. Small enough to cite precisely, large enough to carry one idea. */
export const MAX_CHUNK_CHARS = 1500;

/** Lecture transcript windows, in seconds. About one spoken paragraph. */
export const TRANSCRIPT_WINDOW_SEC = 60;

export async function extractPdfPages(file: Uint8Array): Promise<PageText[]> {
    const pdf = await getDocumentProxy(file);
    const { text } = await extractText(pdf, { mergePages: false });

    return text
        .map((pageText, index) => ({ page: index + 1, text: tidy(pageText) }))
        .filter((page) => page.text.length > 0);
}

/**
 * Slide text, speaker notes and pictures, in slide order.
 *
 * A .pptx is a zip of XML. Slide N lives at ppt/slides/slideN.xml, its pictures are named in
 * ppt/slides/_rels/slideN.xml.rels, and its notes in a notesSlide the same rels file points to.
 */
export async function extractPptxSlides(file: Uint8Array): Promise<SlideContent[]> {
    const zip = await JSZip.loadAsync(file);

    const slidePaths = Object.keys(zip.files)
        .filter((path) => /^ppt\/slides\/slide\d+\.xml$/.test(path))
        .sort((a, b) => slideNumber(a) - slideNumber(b));

    const slides: SlideContent[] = [];

    for (const path of slidePaths) {
        const slide = slideNumber(path);
        const xml = await zip.file(path)!.async('string');
        const rels = await zip.file(`ppt/slides/_rels/slide${slide}.xml.rels`)?.async('string');

        const targets = rels ? relationshipTargets(rels) : [];

        const notesPath = targets.find((target) => target.includes('notesSlides/'));
        const notesXml = notesPath ? await zip.file(resolvePart(notesPath))?.async('string') : null;

        const images: SlideContent['images'] = [];

        for (const target of targets) {
            const mediaType = imageType(target);
            const part = zip.file(resolvePart(target));

            if (!mediaType || !part) continue;

            images.push({ mediaType, base64: await part.async('base64') });
        }

        const notes = notesXml ? textRuns(notesXml) : '';

        slides.push({
            slide,
            text: tidy([textRuns(xml), notes && `Speaker notes: ${notes}`].filter(Boolean).join('\n')),
            images,
        });
    }

    return slides;
}

/** Timed captions for a YouTube lecture, from the captions YouTube already publishes. */
export async function fetchYoutubeSegments(url: string): Promise<TranscriptSegment[]> {
    const items: RawSegment[] = await fetchTranscript(youtubeId(url) ?? url, { lang: 'en', retries: 2 });

    return items
        .map((item) => {
            const start = Number(item.offset);
            const duration = Number(item.duration);

            return {
                startSec: Math.floor(start),
                endSec: Math.ceil(start + duration),
                text: decodeEntities(item.text),
            };
        })
        .filter((segment: TranscriptSegment) => segment.text.trim().length > 0);
}

/** WebVTT or SRT captions, for a lecture whose captions came as a file. */
export function parseCaptions(captions: string): TranscriptSegment[] {
    const cue = /(\d{1,2}:)?(\d{1,2}):(\d{2})[.,](\d{3})\s*-->\s*(\d{1,2}:)?(\d{1,2}):(\d{2})[.,](\d{3})[^\n]*\n([\s\S]*?)(?=\n\s*\n|$)/g;
    const segments: TranscriptSegment[] = [];

    for (const match of captions.replace(/\r/g, '').matchAll(cue)) {
        const startSec = seconds(match[1], match[2]!, match[3]!);
        const endSec = seconds(match[5], match[6]!, match[7]!);
        const text = match[9]!.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ').trim();

        if (text) segments.push({ startSec, endSec, text });
    }

    return segments;
}

/**
 * Captions arrive a few words at a time; a citation needs a passage. Joins consecutive segments
 * into windows, and drops the repeated words auto-generated captions carry across cue boundaries.
 */
export function windowTranscript(
    segments: TranscriptSegment[],
    windowSec = TRANSCRIPT_WINDOW_SEC
): TranscriptSegment[] {
    const windows: TranscriptSegment[] = [];
    let current: TranscriptSegment | null = null;

    for (const segment of segments) {
        if (!current || segment.startSec - current.startSec >= windowSec) {
            if (current) windows.push(current);
            current = { ...segment };
            continue;
        }

        current.endSec = Math.max(current.endSec, segment.endSec);

        if (!current.text.endsWith(segment.text)) current.text += ` ${segment.text}`;
    }

    if (current) windows.push(current);

    return windows;
}

/** Splits long text at paragraph, then sentence, boundaries so no excerpt exceeds the limit. */
export function splitText(text: string, max = MAX_CHUNK_CHARS): string[] {
    if (text.length <= max) return [text];

    const pieces: string[] = [];
    let current = '';

    for (const sentence of text.split(/(?<=[.!?])\s+|\n{2,}/)) {
        if (current && current.length + sentence.length + 1 > max) {
            pieces.push(current);
            current = '';
        }

        // A single "sentence" longer than the limit (a table, a code listing) is cut hard.
        for (let start = 0; start < sentence.length; start += max) {
            const part = sentence.slice(start, start + max);
            current = current ? `${current} ${part}` : part;

            if (current.length >= max) {
                pieces.push(current);
                current = '';
            }
        }
    }

    if (current.trim()) pieces.push(current);

    return pieces.map((piece) => piece.trim()).filter(Boolean);
}

/** A YouTube address that opens the lecture at a given second. */
export function youtubeAt(url: string, startSec: number): string {
    const id = youtubeId(url);

    return id ? `https://www.youtube.com/watch?v=${id}&t=${startSec}s` : url;
}

export function youtubeId(url: string): string | null {
    const match = url.match(/(?:youtu\.be\/|v=|\/embed\/|\/shorts\/|\/live\/)([A-Za-z0-9_-]{11})/);

    return match ? match[1]! : /^[A-Za-z0-9_-]{11}$/.test(url) ? url : null;
}

function slideNumber(path: string): number {
    return Number(path.match(/slide(\d+)\.xml$/)![1]);
}

function relationshipTargets(rels: string): string[] {
    return [...rels.matchAll(/Target="([^"]+)"/g)].map((match) => match[1]!);
}

/** Rels targets are relative to ppt/slides/, e.g. "../media/image3.png". */
function resolvePart(target: string): string {
    return target.startsWith('../') ? `ppt/${target.slice(3)}` : `ppt/slides/${target}`;
}

function imageType(path: string): SlideContent['images'][number]['mediaType'] | null {
    if (/\.png$/i.test(path)) return 'image/png';
    if (/\.jpe?g$/i.test(path)) return 'image/jpeg';
    if (/\.gif$/i.test(path)) return 'image/gif';
    return null;
}

/** The visible text in DrawingML: every <a:t> run, one paragraph per <a:p>. */
function textRuns(xml: string): string {
    return xml
        .split(/<\/a:p>/)
        .map((paragraph) =>
            [...paragraph.matchAll(/<a:t>([^<]*)<\/a:t>/g)]
                .map((run) => decodeEntities(run[1]!))
                .join('')
        )
        .filter((line) => line.trim())
        .join('\n');
}

function decodeEntities(text: string): string {
    return text
        .replace(/&amp;#39;|&#39;|&apos;/g, "'")
        .replace(/&quot;/g, '"')
        .replace(/&lt;/g, '<')
        .replace(/&gt;/g, '>')
        .replace(/&amp;/g, '&');
}

function tidy(text: string): string {
    return text
        .replace(/[ \t]+/g, ' ')
        .replace(/\n{3,}/g, '\n\n')
        .trim();
}

function seconds(hours: string | undefined, minutes: string, secs: string): number {
    return (hours ? Number(hours.slice(0, -1)) * 3600 : 0) + Number(minutes) * 60 + Number(secs);
}
