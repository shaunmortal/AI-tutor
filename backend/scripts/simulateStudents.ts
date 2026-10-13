/**
 * Offline personalization evaluation (Track D): simulated learners, multiple sessions, the app's
 * REAL engines, no database, no AI.
 *
 * What is real here:
 *   - topic + difficulty  planSession()           (recommendationEngine, which calls chooseDifficulty)
 *   - question            QuestionSelector.select (the real class, fed an in-memory repository)
 *   - mastery estimate    summariseAttempts() + computeMastery(), recomputed from the attempt log
 *                         exactly as practiceService does after every answer
 *
 * What is simulated: the learner. Each has a hidden skill per topic (logits). P(correct) is a
 * logistic of skill minus question difficulty (1PL IRT). Practice raises skill, more after a wrong
 * answer (which the app explains), scaled by item information 4p(1-p) so questions far too easy or
 * far too hard teach little. Skill decays towards its starting level between sessions.
 *
 * Not used: the SRS engine and planEngine. Neither is consulted by GET /recommendations/next
 * (adaptiveController), which is the path that picks the next topic and question.
 *
 * Run: npm run simulate
 */
import { mkdirSync, writeFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import type { PracticeQuestion, QuestionRepository } from '@/repositories/questionRepository.js';
import { QuestionSelector } from '@/services/adaptive/questionSelector.js';
import { type TopicCandidate, planSession } from '@/services/adaptive/recommendationEngine.js';
import {
    type AttemptRecord,
    computeMastery,
    summariseAttempts,
} from '@/services/learner/masteryEngine.js';

type Difficulty = 'easy' | 'medium' | 'hard';
type Policy = 'adaptive' | 'baseline';
type ProfileName = 'novice' | 'uneven' | 'forgetful' | 'advanced';

export const SIM_CONFIG = {
    topicCount: 8,
    questionsPerDifficulty: 5, // 15 per topic
    difficultyLogit: { easy: -1.2, medium: 0, hard: 1.2 } as Record<Difficulty, number>,
    sessions: 10,
    daysBetweenSessions: 2, // 10 sessions over 20 days
    questionsPerSession: 15,
    seeds: [1, 2, 3, 4, 5],
    startDate: Date.UTC(2026, 0, 1, 9),
    wrongAnswerLearningBoost: 1.5,
};

const DAY = 86_400_000;

export function mulberry32(seed: number): () => number {
    let a = seed >>> 0;
    return () => {
        a = (a + 0x6d2b79f5) >>> 0;
        let t = a;
        t = Math.imul(t ^ (t >>> 15), t | 1);
        t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
        return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
}

const sigmoid = (x: number) => 1 / (1 + Math.exp(-x));
const mean = (xs: number[]) => xs.reduce((s, x) => s + x, 0) / Math.max(1, xs.length);
const r2 = (x: number) => Math.round(x * 100) / 100;

// ---------------------------------------------------------------- course

interface SimQuestion {
    id: string;
    topicId: string;
    difficulty: Difficulty;
    b: number; // IRT difficulty parameter
}

interface Course {
    topics: { id: string; name: string }[];
    pool: Map<string, SimQuestion[]>;
}

/** Fixed course, independent of learner seed, so every run sees the same bank. */
export function buildCourse(): Course {
    const rng = mulberry32(424242);
    const topics = Array.from({ length: SIM_CONFIG.topicCount }, (_, i) => ({
        id: `t${i + 1}`,
        name: `Topic ${String(i + 1).padStart(2, '0')}`,
    }));
    const pool = new Map<string, SimQuestion[]>();
    for (const topic of topics) {
        const questions: SimQuestion[] = [];
        for (const difficulty of ['easy', 'medium', 'hard'] as const) {
            for (let j = 0; j < SIM_CONFIG.questionsPerDifficulty; j++) {
                questions.push({
                    id: `${topic.id}-${difficulty[0]}${j + 1}`,
                    topicId: topic.id,
                    difficulty,
                    b: SIM_CONFIG.difficultyLogit[difficulty] + (rng() - 0.5) * 0.6,
                });
            }
        }
        pool.set(topic.id, questions);
    }
    return { topics, pool };
}

/**
 * In-memory stand-in for QuestionRepository: only the two methods QuestionSelector calls, following
 * the same rules as the Supabase queries (filter by topic, optional difficulty, exclude ids, limit).
 * The SQL has no ORDER BY, so Postgres returns rows in storage order; this returns pool order.
 */
function memoryQuestionRepository(course: Course, seenIds: Map<string, Set<string>>) {
    return {
        async findAttemptedIds(_userId: string, topicId: string) {
            // Most recently answered first, as the real query orders by attempted_at desc.
            return [...(seenIds.get(topicId) ?? [])].reverse();
        },
        async findForPractice(o: {
            topicId: string;
            difficulty: Difficulty | null;
            exclude: string[];
            only?: string[];
            limit: number;
        }): Promise<PracticeQuestion[]> {
            const exclude = new Set(o.exclude);
            const only = o.only ? new Set(o.only) : null;
            return (course.pool.get(o.topicId) ?? [])
                .filter(
                    (q) =>
                        (!o.difficulty || q.difficulty === o.difficulty) &&
                        !exclude.has(q.id) &&
                        (!only || only.has(q.id))
                )
                .slice(0, o.limit)
                .map((q) => ({
                    id: q.id,
                    topic_id: q.topicId,
                    question_type: 'mcq',
                    body: q.id,
                    options: null,
                    difficulty: q.difficulty,
                    marks: 1,
                }));
        },
    } as unknown as QuestionRepository; // the real class has a private db field; we only need its shape
}

// ---------------------------------------------------------------- learners

interface Profile {
    name: ProfileName;
    initialSkill: (topicIndex: number, rng: () => number) => number;
    learningRate: number;
    decayPerDay: number; // fraction of above-baseline skill lost per idle day
}

const gauss = (rng: () => number) =>
    Math.sqrt(-2 * Math.log(1 - rng())) * Math.cos(2 * Math.PI * rng());

export const PROFILES: Profile[] = [
    { name: 'novice', initialSkill: (_, r) => -1.6 + 0.3 * gauss(r), learningRate: 0.1, decayPerDay: 0.02 },
    {
        name: 'uneven',
        // Even topics strong, odd topics weak.
        initialSkill: (i, r) => (i % 2 === 0 ? 1.4 : -1.6) + 0.3 * gauss(r),
        learningRate: 0.1,
        decayPerDay: 0.02,
    },
    { name: 'forgetful', initialSkill: (_, r) => -0.6 + 0.4 * gauss(r), learningRate: 0.25, decayPerDay: 0.12 },
    { name: 'advanced', initialSkill: (_, r) => 1.6 + 0.3 * gauss(r), learningRate: 0.06, decayPerDay: 0.01 },
];

/** True mastery, 0-100: expected accuracy over the topic's whole bank. Same scale as the app's score. */
function trueMastery(skill: number, questions: SimQuestion[]): number {
    return 100 * mean(questions.map((q) => sigmoid(skill - q.b)));
}

// ---------------------------------------------------------------- one run

export interface SessionPoint {
    session: number;
    trueMastery: number;
    estimatedMastery: number;
    calibrationError: number;
    weakTopicTrueMastery: number;
    accuracy: number;
    withinSessionRepeatRate: number;
    crossSessionRepeatRate: number;
}

export interface RunResult {
    profile: ProfileName;
    policy: Policy;
    seed: number;
    startTrueMastery: number;
    endTrueMastery: number;
    trueSkillGain: number;
    weakTopicIds: string[];
    weakTopicGain: number;
    initialCalibrationError: number;
    finalCalibrationError: number;
    withinSessionRepeatRate: number;
    crossSessionRepeatRate: number;
    series: SessionPoint[];
}

export async function runOne(profile: Profile, policy: Policy, seed: number): Promise<RunResult> {
    const course = buildCourse();
    const { topics, pool } = course;
    // True mastery is measured on the original bank, so questions written later by the top-up do
    // not move the yardstick.
    const yardstick = new Map([...pool].map(([id, questions]) => [id, [...questions]]));
    // Topics that ran out of new questions; the app writes FIRST_BATCH (3) more in the background,
    // which become available from the next session. Adaptive policy only: it is the app's behaviour.
    const pendingTopUps: { topicId: string; difficulty: Difficulty }[] = [];
    let toppedUp = 0;

    // Same seed => same initial skills and same answer stream for both policies. Choice randomness
    // (baseline only) gets its own stream so it does not shift the answer stream.
    const skillRng = mulberry32(seed * 1000 + PROFILES.indexOf(profile));
    const answerRng = mulberry32(seed * 7919 + 17);
    const choiceRng = mulberry32(seed * 104729 + 3);

    const baseSkill = new Map(topics.map((t, i) => [t.id, profile.initialSkill(i, skillRng)]));
    const skill = new Map(baseSkill);
    const attempts = new Map<string, AttemptRecord[]>(topics.map((t) => [t.id, []])); // newest first
    const seenIds = new Map<string, Set<string>>(topics.map((t) => [t.id, new Set()]));
    const selector = new QuestionSelector(memoryQuestionRepository(course, seenIds));

    const tm = (id: string) => trueMastery(skill.get(id)!, yardstick.get(id)!);
    const startTrue = new Map(topics.map((t) => [t.id, tm(t.id)]));
    const weakTopicIds = [...topics]
        .sort((a, b) => startTrue.get(a.id)! - startTrue.get(b.id)! || a.id.localeCompare(b.id))
        .slice(0, 3)
        .map((t) => t.id);

    const estimate = (id: string, now: Date) =>
        computeMastery(summariseAttempts(attempts.get(id)!, now)).score;
    const calibration = (now: Date) => mean(topics.map((t) => Math.abs(estimate(t.id, now) - tm(t.id))));

    const initialCalibrationError = calibration(new Date(SIM_CONFIG.startDate));
    const series: SessionPoint[] = [];
    let served = 0;
    let withinRepeats = 0;
    let crossRepeats = 0;

    for (let s = 0; s < SIM_CONFIG.sessions; s++) {
        const sessionStart = SIM_CONFIG.startDate + s * SIM_CONFIG.daysBetweenSessions * DAY;

        // Forgetting since the previous session: above-baseline skill decays towards the start level.
        if (s > 0) {
            const keep = Math.exp(-profile.decayPerDay * SIM_CONFIG.daysBetweenSessions);
            for (const t of topics) {
                const base = baseSkill.get(t.id)!;
                skill.set(t.id, base + (skill.get(t.id)! - base) * keep);
            }
        }

        for (const { topicId, difficulty } of pendingTopUps.splice(0)) {
            for (let k = 0; k < 3; k++) {
                toppedUp++;
                pool.get(topicId)!.push({
                    id: `${topicId}-new${toppedUp}`,
                    topicId,
                    difficulty,
                    b: SIM_CONFIG.difficultyLogit[difficulty],
                });
            }
        }

        const seenBeforeSession = new Set([...seenIds.values()].flatMap((set) => [...set]));
        const seenThisSession = new Set<string>();
        let answered = 0;
        let correctCount = 0;
        let sessionWithin = 0;
        let sessionCross = 0;

        const answer = (q: SimQuestion) => {
            const now = new Date(sessionStart + answered * 60_000);
            const before = skill.get(q.topicId)!;
            const p = sigmoid(before - q.b);
            const correct = answerRng() < p;
            const boost = correct ? 1 : SIM_CONFIG.wrongAnswerLearningBoost;
            skill.set(q.topicId, before + profile.learningRate * boost * 4 * p * (1 - p));

            if (seenThisSession.has(q.id)) sessionWithin++;
            else if (seenBeforeSession.has(q.id)) sessionCross++;
            seenThisSession.add(q.id);
            // Re-inserted so the set's order is order of latest attempt.
            seenIds.get(q.topicId)!.delete(q.id);
            seenIds.get(q.topicId)!.add(q.id);
            attempts.get(q.topicId)!.unshift({
                isCorrect: correct,
                difficulty: q.difficulty,
                attemptedAt: now.toISOString(),
            });
            answered++;
            if (correct) correctCount++;
        };

        while (answered < SIM_CONFIG.questionsPerSession) {
            const remaining = SIM_CONFIG.questionsPerSession - answered;

            if (policy === 'baseline') {
                const topic = topics[Math.floor(choiceRng() * topics.length)]!;
                const questions = pool.get(topic.id)!;
                answer(questions[Math.floor(choiceRng() * questions.length)]!);
                continue;
            }

            // ADAPTIVE: rebuild candidates from the log the way TopicRepository.findCandidatesForLearner
            // reads concept_mastery, then make the same two calls adaptiveController makes.
            const now = new Date(sessionStart + answered * 60_000);
            const candidates: TopicCandidate[] = topics.map((t) => {
                const log = attempts.get(t.id)!;
                if (log.length === 0) {
                    return { topicId: t.id, name: t.name, weight: 1, masteryScore: null, recentAccuracy: null, recentAttempts: 0, totalAttempts: 0, daysSinceLastAttempt: null };
                }
                const evidence = summariseAttempts(log, now);
                return {
                    topicId: t.id,
                    name: t.name,
                    weight: 1,
                    masteryScore: computeMastery(evidence).score,
                    recentAccuracy: evidence.recentCorrect / evidence.recentAttempts,
                    recentAttempts: evidence.recentAttempts,
                    totalAttempts: evidence.totalAttempts,
                    daysSinceLastAttempt: evidence.daysSinceLastAttempt,
                };
            });

            const plan = planSession({ candidates, secondsPerQuestion: null, availableMinutes: null })!;
            const selection = await selector.select({
                userId: 'sim',
                topicId: plan.candidate.topicId,
                difficulty: plan.difficulty,
                count: Math.min(plan.requestedQuestionCount, remaining),
            });
            if (selection.questions.length === 0) throw new Error(`empty selection for ${plan.candidate.topicId}`);
            if (
                selection.repeats > 0 &&
                !pendingTopUps.some((t) => t.topicId === plan.candidate.topicId)
            ) {
                pendingTopUps.push({ topicId: plan.candidate.topicId, difficulty: plan.difficulty });
            }

            const byId = new Map(pool.get(plan.candidate.topicId)!.map((q) => [q.id, q]));
            for (const q of selection.questions) answer(byId.get(q.id)!);
        }

        served += answered;
        withinRepeats += sessionWithin;
        crossRepeats += sessionCross;

        const end = new Date(sessionStart + answered * 60_000);
        const trueNow = mean(topics.map((t) => tm(t.id)));
        const estNow = mean(topics.map((t) => estimate(t.id, end)));
        series.push({
            session: s + 1,
            trueMastery: r2(trueNow),
            estimatedMastery: r2(estNow),
            calibrationError: r2(calibration(end)),
            weakTopicTrueMastery: r2(mean(weakTopicIds.map(tm))),
            accuracy: r2(correctCount / answered),
            withinSessionRepeatRate: r2(sessionWithin / answered),
            crossSessionRepeatRate: r2(sessionCross / answered),
        });
    }

    const startTrueMastery = mean([...startTrue.values()]);
    const endTrueMastery = mean(topics.map((t) => tm(t.id)));

    return {
        profile: profile.name,
        policy,
        seed,
        startTrueMastery: r2(startTrueMastery),
        endTrueMastery: r2(endTrueMastery),
        trueSkillGain: r2(endTrueMastery - startTrueMastery),
        weakTopicIds,
        weakTopicGain: r2(mean(weakTopicIds.map((id) => tm(id) - startTrue.get(id)!))),
        initialCalibrationError: r2(initialCalibrationError),
        finalCalibrationError: series.at(-1)!.calibrationError,
        withinSessionRepeatRate: r2(withinRepeats / served),
        crossSessionRepeatRate: r2(crossRepeats / served),
        series,
    };
}

// ---------------------------------------------------------------- aggregate

export interface Summary {
    profile: ProfileName;
    policy: Policy;
    trueSkillGain: number;
    weakTopicGain: number;
    initialCalibrationError: number;
    finalCalibrationError: number;
    withinSessionRepeatRate: number;
    crossSessionRepeatRate: number;
    /** Per-session means across seeds, for charts. */
    series: SessionPoint[];
}

export async function runSimulation(seeds = SIM_CONFIG.seeds) {
    const runs: RunResult[] = [];
    for (const profile of PROFILES) {
        for (const policy of ['adaptive', 'baseline'] as const) {
            for (const seed of seeds) runs.push(await runOne(profile, policy, seed));
        }
    }

    const summaries: Summary[] = [];
    for (const profile of PROFILES) {
        for (const policy of ['adaptive', 'baseline'] as const) {
            const group = runs.filter((r) => r.profile === profile.name && r.policy === policy);
            const avg = (f: (r: RunResult) => number) => r2(mean(group.map(f)));
            const series = group[0]!.series.map((_, i) => {
                const pts = group.map((r) => r.series[i]!);
                const m = (k: keyof SessionPoint) => r2(mean(pts.map((p) => p[k])));
                return {
                    session: i + 1,
                    trueMastery: m('trueMastery'),
                    estimatedMastery: m('estimatedMastery'),
                    calibrationError: m('calibrationError'),
                    weakTopicTrueMastery: m('weakTopicTrueMastery'),
                    accuracy: m('accuracy'),
                    withinSessionRepeatRate: m('withinSessionRepeatRate'),
                    crossSessionRepeatRate: m('crossSessionRepeatRate'),
                };
            });
            summaries.push({
                profile: profile.name,
                policy,
                trueSkillGain: avg((r) => r.trueSkillGain),
                weakTopicGain: avg((r) => r.weakTopicGain),
                initialCalibrationError: avg((r) => r.initialCalibrationError),
                finalCalibrationError: avg((r) => r.finalCalibrationError),
                withinSessionRepeatRate: avg((r) => r.withinSessionRepeatRate),
                crossSessionRepeatRate: avg((r) => r.crossSessionRepeatRate),
                series,
            });
        }
    }
    return { config: { ...SIM_CONFIG, profiles: PROFILES.map(({ name, learningRate, decayPerDay }) => ({ name, learningRate, decayPerDay })) }, summaries, runs };
}

function printTable(summaries: Summary[]) {
    const pct = (x: number) => `${(x * 100).toFixed(1)}%`;
    const rows = summaries.map((s) => ({
        profile: s.profile,
        policy: s.policy,
        'skill gain': s.trueSkillGain.toFixed(2),
        'weak-topic gain': s.weakTopicGain.toFixed(2),
        'calib err s1': s.series[0]!.calibrationError.toFixed(2),
        'calib err s5': s.series[4]!.calibrationError.toFixed(2),
        'calib err s10': s.finalCalibrationError.toFixed(2),
        'repeat in-session': pct(s.withinSessionRepeatRate),
        'repeat cross-session': pct(s.crossSessionRepeatRate),
    }));
    const cols = Object.keys(rows[0]!) as (keyof (typeof rows)[0])[];
    const width = cols.map((c) => Math.max(c.length, ...rows.map((r) => r[c].length)));
    const line = (cells: string[]) => cells.map((c, i) => c.padEnd(width[i]!)).join(' | ');
    console.log(line(cols));
    console.log(width.map((w) => '-'.repeat(w)).join('-|-'));
    for (const r of rows) console.log(line(cols.map((c) => r[c])));
    console.log('\nGains and calibration error are in mastery points (0-100). Means over seeds', SIM_CONFIG.seeds.join(','));
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
    const result = await runSimulation();
    printTable(result.summaries);
    const dir = new URL('./output/', import.meta.url);
    mkdirSync(dir, { recursive: true });
    const file = new URL('simulation-results.json', dir);
    writeFileSync(file, JSON.stringify(result, null, 2));
    console.log(`\nWrote ${file.pathname}`);
}
