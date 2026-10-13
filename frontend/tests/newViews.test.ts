import { describe, expect, it } from 'vitest';
import { isDeck } from '@/components/Citations/deck';
import { bandOf, layers, layoutCourseMap } from '@/components/CourseGraph/layout';
import { briefParts, pickVoice } from '@/pages/SmartRevision/brief';
import type { CourseMap, FlashcardTopic } from '@/types/api';

const topic = (id: string, prereqs: string[] = []) => ({
    id,
    name: id,
    keyConcepts: [],
    excerptCount: 1,
    prerequisites: prereqs.map((p) => ({ id: p, name: p })),
});

describe('course map layout', () => {
    it('puts each topic one layer below its deepest prerequisite', () => {
        const l = layers([
            topic('a'),
            topic('b', ['a']),
            topic('c', ['a', 'b']),
            topic('d', ['zz']),
        ]);
        expect([l.get('a'), l.get('b'), l.get('c'), l.get('d')]).toEqual([0, 1, 2, 0]);
    });

    it('survives a cycle', () => {
        const l = layers([topic('a', ['b']), topic('b', ['a'])]);
        expect(l.size).toBe(2);
    });

    it('draws arrows downward only, between subjects too', () => {
        const map: CourseMap = {
            curriculum: null,
            subjects: [
                { id: 's1', name: 'S1', topics: [topic('a'), topic('b', ['a'])] },
                {
                    id: 's2',
                    name: 'S2',
                    topics: [topic('c', ['b']), topic('x', ['y']), topic('y', ['x'])],
                },
            ],
        };
        const g = layoutCourseMap(map);
        const at = new Map(g.nodes.map((n) => [n.topic.id, n]));
        // The x/y cycle keeps one of its two arrows, whichever does not point up.
        expect(g.edges.map((e) => `${e.from}>${e.to}`).sort()).toEqual(['a>b', 'b>c', 'y>x']);
        expect(at.get('c')!.y).toBeGreaterThan(at.get('b')!.y);
        expect(at.get('c')!.x).toBeGreaterThan(at.get('b')!.x);
        expect(g.width).toBeGreaterThan(at.get('c')!.x);
    });

    it('bands mastery', () => {
        expect([bandOf(null), bandOf(20), bandOf(55), bandOf(90)]).toEqual([
            'none',
            'weak',
            'mid',
            'strong',
        ]);
    });
});

describe('audio brief', () => {
    const topics = [
        {
            topicId: 't',
            name: 'Heaps',
            reason: 'due',
            dueOn: null,
            masteryScore: null,
            cards: [{ id: 'c1', front: 'Q?', back: 'A.', sourceChunkId: null, citations: [] }],
        },
    ] as FlashcardTopic[];

    it('reads the intro, the topic, then each card', () => {
        const parts = briefParts(topics, false);
        expect(parts.map((p) => p.cardId)).toEqual([null, null, 'c1']);
        expect(parts[2]!.text).toBe('Question: Q? Answer: A.');
    });

    it('uses Hindi only when a Hindi voice exists', () => {
        expect(pickVoice([{ lang: 'en-IN' }, { lang: 'hi_IN' }], 'hindi')).toEqual({
            lang: 'hi-IN',
            index: 1,
        });
        expect(pickVoice([{ lang: 'en-US' }, { lang: 'en-IN' }], 'hindi')).toEqual({
            lang: 'en-IN',
            index: 1,
        });
        expect(pickVoice([{ lang: 'hi-IN' }, { lang: 'en-IN' }], 'english').lang).toBe('en-IN');
    });
});

describe('slide excerpts', () => {
    const base = { sourceTitle: 'Deck', location: 'slide 4', excerpt: '' };
    it('offers a .pptx deck as a download, a PDF as a page', () => {
        expect(isDeck({ ...base, slide: 4, url: 'https://x.co/o/sign/b/deck.pptx?token=1' })).toBe(
            true
        );
        expect(
            isDeck({ ...base, slide: 4, url: 'https://x.co/o/sign/b/deck.pdf?token=1#page=4' })
        ).toBe(false);
        expect(isDeck({ ...base, slide: null, url: 'https://youtu.be/x?t=3' })).toBe(false);
    });
});
