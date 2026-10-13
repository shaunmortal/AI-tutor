import { describe, expect, it } from 'vitest';
import { matches, summarise } from '../../scripts/evalMetrics.js';

const at = (sourceTitle: string, extra: { page?: number; startSec?: number; endSec?: number }) => ({
    sourceTitle,
    location: '',
    page: extra.page ?? null,
    startSec: extra.startSec ?? null,
    endSec: extra.endSec ?? null,
});

describe('eval matching', () => {
    it('matches a page exactly and a video range by overlap', () => {
        expect(matches(at('Lecture 1 notes', { page: 3 }), [{ source: 'Lecture 1 notes', page: 3 }])).toBe(true);
        expect(matches(at('Lecture 1 notes', { page: 4 }), [{ source: 'Lecture 1 notes', page: 3 }])).toBe(false);
        expect(
            matches(at('Lecture 2 video', { startSec: 600, endSec: 660 }), [
                { source: 'Lecture 2 video', startSec: 640, endSec: 700 },
            ])
        ).toBe(true);
        expect(
            matches(at('Lecture 2 video', { startSec: 100, endSec: 160 }), [
                { source: 'Lecture 2 video', startSec: 640, endSec: 700 },
            ])
        ).toBe(false);
        expect(matches(at('Lecture 2 notes', { page: 3 }), [{ source: 'Lecture 1 notes', page: 3 }])).toBe(false);
    });

    it('summarises rates over the right subsets', () => {
        const summary = summarise([
            { type: 'in_material', retrievalHit: true, citationHit: true, citationPrecision: 1, refused: false },
            { type: 'in_material', retrievalHit: true, citationHit: false, citationPrecision: 0, refused: false },
            { type: 'off_material', retrievalHit: null, citationHit: null, citationPrecision: null, refused: true },
        ]);

        expect(summary).toMatchObject({
            retrievalHitRatePct: 100,
            citationAccuracyPct: 50,
            citationPrecisionPct: 50,
            falseRefusalRatePct: 0,
            correctRefusalRatePct: 100,
        });
    });
});
