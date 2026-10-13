/**
 * Evaluates the source-grounded tutor on a team-built test set, end to end.
 *
 * Runs exactly what the tutor runs (answerFromMaterial: retrieval, the grounded prompt, and the
 * citation checks) against real course material — MIT 6.006 Introduction to Algorithms, lectures
 * 1–3: the lecture-notes PDFs and the lecture videos — and measures:
 *
 *   retrieval hit rate     an excerpt at an expected location was among those retrieved
 *   citation accuracy      the answer cited an excerpt at an expected location
 *   citation precision     share of cited excerpts that are at an expected location
 *   false refusal rate     in-material questions answered "not covered"
 *   correct refusal rate   off-material questions answered "not covered"
 *
 * and writes one JSON line per question with the retrieved contexts, so eval/ragas_eval.py can
 * score faithfulness, answer relevancy and context precision/recall with a standard framework.
 *
 * Usage (needs backend/.env):
 *   npm run eval:grounding -- --user <learner uuid> [--ingest]
 *
 * --ingest uploads the six sources for that learner first and waits until they are read; titles
 * must match the test set's "source" names, which is why the script does it rather than a person.
 */
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { adminDb } from '@/config/database.js';
import { CurriculumRepository } from '@/repositories/curriculumRepository.js';
import { SourceRepository } from '@/repositories/sourceRepository.js';
import { answerFromMaterial } from '@/services/ai/aiService.js';
import { claudeProvider } from '@/services/ai/providers/claudeProvider.js';
import type { Citation } from '@/services/sources/sourceService.js';
import { SourceService } from '@/services/sources/sourceService.js';
import { type Expected, matches, summarise } from './evalMetrics.js';

const ROOT = path.resolve(import.meta.dirname, '../..');
const OCW = 'https://ocw.mit.edu/courses/6-006-introduction-to-algorithms-spring-2020';

/** The course material, with the exact titles the test set refers to. */
export const COURSE = [
    { title: 'Lecture 1 notes', pdf: `${OCW}/477c78e0af2df61fa205bcc6cb613ceb_MIT6_006S20_lec1.pdf` },
    { title: 'Lecture 2 notes', pdf: `${OCW}/79a07dc1cb47d76dae2ffedc701e3d2b_MIT6_006S20_lec2.pdf` },
    { title: 'Lecture 3 notes', pdf: `${OCW}/6d1ae5278d02bbecb5c4428928b24194_MIT6_006S20_lec3.pdf` },
    { title: 'Lecture 1 video', video: 'https://www.youtube.com/watch?v=ZA-tUyM_y7s' },
    { title: 'Lecture 2 video', video: 'https://www.youtube.com/watch?v=CHhwJjR0mZA' },
    { title: 'Lecture 3 video', video: 'https://www.youtube.com/watch?v=oS9aPzUNG-s' },
] as const;

interface TestItem {
    id: string;
    type: 'in_material' | 'off_material' | 'figure';
    question: string;
    reference: string;
    expected: Expected[];
}

function argument(name: string): string | undefined {
    const index = process.argv.indexOf(`--${name}`);
    return index === -1 ? undefined : process.argv[index + 1];
}

async function main() {
    const userId = argument('user');
    if (!userId) throw new Error('Pass --user <learner uuid>.');

    const sourceRepo = new SourceRepository(adminDb);
    const service = new SourceService(claudeProvider(), sourceRepo, new CurriculumRepository(adminDb));

    if (process.argv.includes('--ingest')) await ingest(service, userId);

    // Locations come back as labels ("p. 3", "12:05"); the raw numbers are needed to match ranges.
    const chunkRows = new Map(
        (await sourceRepo.listChunks(userId)).map((row) => [row.id, row])
    );
    const locate = (citation: Citation) => {
        const row = chunkRows.get(citation.chunkId);
        return { ...citation, page: row?.page ?? row?.slide ?? null, startSec: row?.start_sec ?? null, endSec: row?.end_sec ?? null };
    };

    const items = JSON.parse(readFileSync(path.join(ROOT, 'eval/testset.json'), 'utf8')) as TestItem[];
    const rows: Record<string, unknown>[] = [];

    for (const item of items) {
        const result = await answerFromMaterial(claudeProvider(), service, userId, item.question, {
            topicName: null,
            subjectName: null,
            masteryScore: null,
            attemptsOnTopic: 0,
            recentAccuracyPercent: null,
            recentMistakes: [],
            currentQuestion: null,
        });

        const retrieved = result.retrieved.map(locate);
        const cited = result.citations.map(locate);
        const inMaterial = item.type !== 'off_material';

        rows.push({
            id: item.id,
            type: item.type,
            question: item.question,
            reference: item.reference,
            answer: result.text,
            grounding: result.grounding,
            contexts: retrieved.map((c) => c.excerpt),
            retrieved: retrieved.map((c) => `${c.sourceTitle}, ${c.location}`),
            cited: cited.map((c) => `${c.sourceTitle}, ${c.location}`),
            expected: item.expected,
            retrievalHit: inMaterial ? retrieved.some((c) => matches(c, item.expected)) : null,
            citationHit: inMaterial ? cited.some((c) => matches(c, item.expected)) : null,
            citationPrecision:
                inMaterial && cited.length > 0
                    ? cited.filter((c) => matches(c, item.expected)).length / cited.length
                    : null,
            refused: result.grounding === 'none',
        });

        process.stdout.write(`${item.id} ${item.type} grounding=${result.grounding} cited=${cited.length}\n`);
    }

    const summary = summarise(rows);
    const out = path.join(ROOT, 'eval/results');
    mkdirSync(out, { recursive: true });

    writeFileSync(path.join(out, 'grounding.jsonl'), rows.map((row) => JSON.stringify(row)).join('\n') + '\n');
    writeFileSync(path.join(out, 'grounding-summary.json'), JSON.stringify(summary, null, 2));

    console.table(summary);
}

async function ingest(service: SourceService, userId: string) {
    for (const item of COURSE) {
        if ('pdf' in item) {
            const file = new Uint8Array(await (await fetch(item.pdf)).arrayBuffer());
            await service.uploadFile({ userId, kind: 'pdf', title: item.title, file, fileName: item.title });
        } else {
            await service.addVideo({ userId, url: item.video, title: item.title });
        }
        console.log(`queued ${item.title}`);
    }

    // Ingestion runs in the background of this process; wait until nothing is processing.
    for (;;) {
        const sources = await service.list(userId);
        const pending = sources.filter((source) => source.status === 'processing');

        if (pending.length === 0) {
            for (const source of sources) {
                console.log(`${source.title}: ${source.status} (${source.excerptCount} excerpts)${source.error ? ` ${source.error}` : ''}`);
            }
            return;
        }

        await new Promise((resolve) => setTimeout(resolve, 3000));
    }
}

if (process.argv[1] && import.meta.url.endsWith(path.basename(process.argv[1]))) {
    main().then(
        () => process.exit(0),
        (error: unknown) => {
            console.error(error);
            process.exit(1);
        }
    );
}
