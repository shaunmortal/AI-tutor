import { describe, expect, it } from 'vitest';
import { checkGrounding } from '@/services/ai/aiService.js';
import {
    parseCaptions,
    splitText,
    windowTranscript,
    youtubeAt,
    youtubeId,
} from '@/services/sources/extractors.js';
import type { Citation } from '@/services/sources/sourceService.js';
import { assignTopics, locationOf, parseCourseMap } from '@/services/sources/sourceService.js';

const excerpt = (n: number): Citation => ({
    n,
    chunkId: `chunk-${n}`,
    sourceId: 'source',
    sourceTitle: 'Book',
    kind: 'pdf',
    modality: 'text',
    location: `p. ${n}`,
    excerpt: `excerpt ${n}`,
});

describe('checkGrounding', () => {
    it('keeps a cited, source-backed answer and returns only the excerpts it cites', () => {
        const result = checkGrounding(
            { grounding: 'source', answer: 'A heap is a tree [2]. It is complete [2].', outsideKnowledge: '' },
            [excerpt(1), excerpt(2)]
        );

        expect(result.grounding).toBe('source');
        expect(result.citations.map((c) => c.n)).toEqual([2]);
        expect(result.text).toContain('[2]');
    });

    it('removes citation numbers that point at excerpts the model was never given', () => {
        const result = checkGrounding(
            { grounding: 'source', answer: 'True [1]. Also true [9].', outsideKnowledge: '' },
            [excerpt(1)]
        );

        expect(result.text).not.toContain('[9]');
        expect(result.citations.map((c) => c.n)).toEqual([1]);
    });

    it('does not let an uncited answer pass as coming from the course', () => {
        const result = checkGrounding(
            { grounding: 'source', answer: 'Quicksort is O(n log n).', outsideKnowledge: '' },
            [excerpt(1)]
        );

        expect(result.grounding).toBe('none');
        expect(result.citations).toEqual([]);
        expect(result.text).toContain('not covered in your course material');
        expect(result.text).toContain('General knowledge, not from your course material');
    });

    it('declines clearly when the material does not cover the question', () => {
        const result = checkGrounding(
            { grounding: 'none', answer: 'ignored', outsideKnowledge: 'This is about cooking.' },
            [excerpt(1)]
        );

        expect(result.grounding).toBe('none');
        expect(result.text.startsWith('This is not covered in your course material.')).toBe(true);
        expect(result.text).not.toContain('ignored');
    });

    it('keeps partial answers and labels the added knowledge separately', () => {
        const result = checkGrounding(
            { grounding: 'partial', answer: 'Defined here [1].', outsideKnowledge: 'Used in Dijkstra.' },
            [excerpt(1)]
        );

        expect(result.grounding).toBe('partial');
        expect(result.text).toMatch(/Defined here \[1\]\.\n\nGeneral knowledge, not from your course material: Used in Dijkstra\./);
    });

    it('treats a malformed reply as not grounded', () => {
        expect(checkGrounding(null, [excerpt(1)]).grounding).toBe('none');
    });
});

describe('captions', () => {
    it('parses WebVTT and SRT timestamps', () => {
        const vtt = `WEBVTT\n\n00:00:01.000 --> 00:00:04.500\nHello <b>class</b>\n\n01:02:03.250 --> 01:02:05.000\nLater\n`;
        const srt = `1\n00:00:10,000 --> 00:00:12,000\nFirst line\nsecond line\n\n2\n00:00:12,000 --> 00:00:14,000\nNext\n`;

        expect(parseCaptions(vtt)).toEqual([
            { startSec: 1, endSec: 4, text: 'Hello class' },
            { startSec: 3723, endSec: 3725, text: 'Later' },
        ]);
        expect(parseCaptions(srt)[0]).toEqual({ startSec: 10, endSec: 12, text: 'First line second line' });
    });

    it('joins short cues into windows that keep their start time', () => {
        const windows = windowTranscript(
            [
                { startSec: 0, endSec: 5, text: 'a' },
                { startSec: 30, endSec: 35, text: 'b' },
                { startSec: 61, endSec: 66, text: 'c' },
            ],
            60
        );

        expect(windows).toEqual([
            { startSec: 0, endSec: 35, text: 'a b' },
            { startSec: 61, endSec: 66, text: 'c' },
        ]);
    });
});

describe('splitText', () => {
    it('leaves short text alone and never exceeds the limit', () => {
        expect(splitText('short')).toEqual(['short']);

        const long = 'One sentence here. '.repeat(400);
        const pieces = splitText(long, 500);

        expect(pieces.length).toBeGreaterThan(1);
        expect(pieces.every((piece) => piece.length <= 500)).toBe(true);
    });

    it('cuts a single unbroken run hard', () => {
        expect(splitText('x'.repeat(1200), 500).every((piece) => piece.length <= 500)).toBe(true);
    });
});

describe('youtube links', () => {
    it('finds the id in every common form and builds a timestamped link', () => {
        for (const url of [
            'https://www.youtube.com/watch?v=ZA-tUyM_y7s',
            'https://youtu.be/ZA-tUyM_y7s',
            'https://www.youtube.com/embed/ZA-tUyM_y7s',
        ]) {
            expect(youtubeId(url)).toBe('ZA-tUyM_y7s');
        }

        expect(youtubeAt('https://youtu.be/ZA-tUyM_y7s', 125)).toBe(
            'https://www.youtube.com/watch?v=ZA-tUyM_y7s&t=125s'
        );
        expect(youtubeId('https://example.com/video')).toBeNull();
    });
});

describe('course map', () => {
    it('drops duplicate topics and empty subjects, and clamps weights', () => {
        const map = parseCourseMap({
            subjects: [
                {
                    name: 'Sorting',
                    weight: 9,
                    topics: [
                        { name: 'Merge sort', keyConcepts: ['divide'], prerequisites: [], excerptIds: [0, 1] },
                        { name: 'Merge sort', keyConcepts: [], prerequisites: [], excerptIds: [2] },
                    ],
                },
                { name: 'Empty', weight: 1, topics: [] },
            ],
        });

        expect(map.subjects).toHaveLength(1);
        expect(map.subjects[0]!.weight).toBe(5);
        expect(map.subjects[0]!.topics).toHaveLength(1);
    });

    it('refuses an empty map', () => {
        expect(() => parseCourseMap({ subjects: [] })).toThrow();
    });

    it('gives unseen excerpts the topic of their nearest seen neighbour in the same source', () => {
        const chunks = [
            { id: 'a', source_id: 's1' },
            { id: 'b', source_id: 's1' },
            { id: 'c', source_id: 's1' },
            { id: 'd', source_id: 's2' },
        ];

        const assigned = assignTopics(chunks, new Map([[0, 'T1'], [3, 'T2']]));

        expect(assigned).toEqual([
            { chunkId: 'a', topicId: 'T1' },
            { chunkId: 'b', topicId: 'T1' },
            { chunkId: 'c', topicId: 'T1' },
            { chunkId: 'd', topicId: 'T2' },
        ]);
    });

    it('formats locations the way a citation chip shows them', () => {
        expect(locationOf({ page: 42, slide: null, start_sec: null })).toBe('p. 42');
        expect(locationOf({ page: null, slide: 7, start_sec: null })).toBe('slide 7');
        expect(locationOf({ page: null, slide: null, start_sec: 725 })).toBe('12:05');
        expect(locationOf({ page: null, slide: null, start_sec: 3725 })).toBe('1:02:05');
    });
});
