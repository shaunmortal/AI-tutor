/** Pure scoring for evalGrounding.ts, kept apart so it is unit-tested without a database. */
import type { Citation } from '@/services/sources/sourceService.js';

export interface Expected {
    source: string;
    page?: number;
    slide?: number;
    startSec?: number;
    endSec?: number;
}

/** Whether an excerpt sits at one of the locations the answer is known to be in. */
export function matches(citation: Pick<Citation, 'sourceTitle' | 'location'> & { page?: number | null; startSec?: number | null; endSec?: number | null }, expected: Expected[]): boolean {
    return expected.some((place) => {
        if (place.source !== citation.sourceTitle) return false;

        if (place.page !== undefined) return citation.page === place.page;
        if (place.slide !== undefined) return citation.page === place.slide;

        if (place.startSec !== undefined && citation.startSec !== null && citation.startSec !== undefined) {
            const end = citation.endSec ?? citation.startSec;
            // Transcript windows are cut differently from the test set's ranges, so any overlap counts.
            return citation.startSec <= (place.endSec ?? place.startSec) && end >= place.startSec;
        }

        return false;
    });
}

export function summarise(rows: Record<string, unknown>[]) {
    const inMaterial = rows.filter((row) => row.type !== 'off_material');
    const offMaterial = rows.filter((row) => row.type === 'off_material');
    const rate = (xs: Record<string, unknown>[], test: (row: Record<string, unknown>) => boolean) =>
        xs.length === 0 ? null : Math.round((xs.filter(test).length / xs.length) * 1000) / 10;
    const precisions = inMaterial
        .map((row) => row.citationPrecision)
        .filter((value): value is number => typeof value === 'number');

    return {
        questions: rows.length,
        inMaterial: inMaterial.length,
        offMaterial: offMaterial.length,
        retrievalHitRatePct: rate(inMaterial, (row) => row.retrievalHit === true),
        citationAccuracyPct: rate(inMaterial, (row) => row.citationHit === true),
        citationPrecisionPct:
            precisions.length === 0
                ? null
                : Math.round((precisions.reduce((a, b) => a + b, 0) / precisions.length) * 1000) / 10,
        falseRefusalRatePct: rate(inMaterial, (row) => row.refused === true),
        correctRefusalRatePct: rate(offMaterial, (row) => row.refused === true),
    };
}

