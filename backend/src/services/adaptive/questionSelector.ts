import type { PracticeQuestion, QuestionRepository } from '@/repositories/questionRepository.js';

export interface SelectedQuestions {
    questions: PracticeQuestion[];
    /** True when the bank could not fill the request, so the UI can say so honestly. */
    short: boolean;
    /** Which fallback was needed, if any. Useful when a topic looks thin. */
    note: string | null;
    /** How many of the questions are ones the learner has answered before. */
    repeats: number;
}

/** Levels in order of closeness to the one asked for, nearest first. */
const NEAREST: Record<Difficulty, Difficulty[]> = {
    easy: ['medium', 'hard'],
    medium: ['easy', 'hard'],
    hard: ['medium', 'easy'],
};

type Difficulty = 'easy' | 'medium' | 'hard';

/**
 * Turns "5 medium questions on Trees" into actual questions.
 *
 * Three tries, in order of preference:
 *   1. unseen questions at the difficulty the engine asked for
 *   2. unseen questions at the nearest other difficulty — medium before hard for a learner who
 *      asked for easy, so a struggling learner is not handed the hardest questions first
 *   3. questions the learner has seen before, the ones answered longest ago first
 *
 * Repeating a question is a real fallback rather than a failure -- answering something a
 * second time is how spaced repetition works -- but it is reported, because a topic that
 * always falls through to step 3 needs more questions in the bank. Repeats come oldest first:
 * handing back what was answered a minute ago tests short-term memory, not learning, and was
 * measured as the main source of repetition in the simulated-student runs.
 */
export class QuestionSelector {
    private readonly questions: QuestionRepository;

    constructor(questions: QuestionRepository) {
        this.questions = questions;
    }

    async select(options: {
        userId: string;
        topicId: string;
        difficulty: Difficulty;
        count: number;
    }): Promise<SelectedQuestions> {
        // Most recently answered first.
        const attempted = await this.questions.findAttemptedIds(options.userId, options.topicId);

        const chosen = await this.questions.findForPractice({
            topicId: options.topicId,
            difficulty: options.difficulty,
            exclude: attempted,
            limit: options.count,
        });

        let note: string | null = null;
        let repeats = 0;

        for (const level of NEAREST[options.difficulty]) {
            if (chosen.length >= options.count) break;

            const nearby = await this.questions.findForPractice({
                topicId: options.topicId,
                difficulty: level,
                exclude: [...attempted, ...chosen.map((q) => q.id)],
                limit: options.count - chosen.length,
            });

            if (nearby.length > 0) {
                note = `Not enough unseen ${options.difficulty} questions, so some are from other levels.`;
                chosen.push(...nearby);
            }
        }

        if (chosen.length < options.count && attempted.length > 0) {
            const taken = new Set(chosen.map((q) => q.id));
            const oldestFirst = [...attempted].reverse().filter((id) => !taken.has(id));

            const seen = await this.questions.findForPractice({
                topicId: options.topicId,
                difficulty: null,
                exclude: [],
                only: oldestFirst,
                limit: oldestFirst.length,
            });

            const byId = new Map(seen.map((q) => [q.id, q]));
            const seenAgain = oldestFirst
                .map((id) => byId.get(id))
                .filter((q): q is PracticeQuestion => q !== undefined)
                .slice(0, options.count - chosen.length);

            if (seenAgain.length > 0) {
                note = 'You have seen every question on this topic, so some are repeats.';
                chosen.push(...seenAgain);
                repeats = seenAgain.length;
            }
        }

        return {
            questions: chosen.slice(0, options.count),
            short: chosen.length < options.count,
            note,
            repeats,
        };
    }
}
