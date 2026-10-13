import type { PracticeQuestion } from '@/types/api';

/** An answer held in the browser until a paper is submitted: options, a number, or text. */
export type PaperAnswer = number[] | number | string;
export type PaperAnswers = Record<string, PaperAnswer>;

/** One answer as the API takes it: exactly the field its question type is graded on. */
export function answerPayload(questionId: string, answer: PaperAnswer) {
    if (Array.isArray(answer)) return { questionId, selectedOptions: answer };
    if (typeof answer === 'string') return { questionId, text: answer };
    return { questionId, value: answer };
}

/** The answers after typing into a question's box. A cleared box is an unanswered question. */
export function withTyped(
    answers: PaperAnswers,
    question: PracticeQuestion,
    raw: string
): PaperAnswers {
    const next = { ...answers };

    if (raw.trim() === '') delete next[question.id];
    else next[question.id] = question.question_type === 'short' ? raw : Number(raw);

    return next;
}

/** A typed answer held in state, as the input's value. */
export function typedValue(answer: PaperAnswer | undefined): string {
    return typeof answer === 'string' || typeof answer === 'number' ? String(answer) : '';
}

/** The answer key in words, once the server has released it. */
export function describeKey(
    questionType: PracticeQuestion['question_type'],
    options: string[] | null,
    key: unknown
): string {
    if (Array.isArray(key)) {
        return key
            .map((i: number) => `${String.fromCharCode(65 + i)}. ${options?.[i] ?? ''}`)
            .join('; ');
    }
    if (questionType === 'short' && key && typeof key === 'object' && 'answers' in key) {
        return (key as { answers: string[] }).answers.join(' / ');
    }
    if (key && typeof key === 'object' && 'value' in key) {
        const { value, tol } = key as { value: number; tol?: number };
        return tol ? `${value} (± ${tol})` : String(value);
    }
    return '';
}
