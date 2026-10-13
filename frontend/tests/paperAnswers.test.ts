import { describe, expect, it } from 'vitest';
import { answerPayload, describeKey, withTyped } from '@/components/Paper/answers';
import type { PracticeQuestion } from '@/types/api';

const question = (question_type: PracticeQuestion['question_type']): PracticeQuestion => ({
    id: 'q1',
    topic_id: 't1',
    question_type,
    body: 'What?',
    options: null,
    difficulty: 'easy',
    marks: 1,
});

describe('paper answers', () => {
    it('sends exactly the field each question type is graded on', () => {
        expect(answerPayload('q1', [0, 2])).toEqual({ questionId: 'q1', selectedOptions: [0, 2] });
        expect(answerPayload('q1', 4.5)).toEqual({ questionId: 'q1', value: 4.5 });
        expect(answerPayload('q1', 'inertia')).toEqual({ questionId: 'q1', text: 'inertia' });
    });

    it('keeps short answers as text, numbers as numbers, and a cleared box as unanswered', () => {
        expect(withTyped({}, question('short'), '42 apples')).toEqual({ q1: '42 apples' });
        expect(withTyped({}, question('numeric'), '42')).toEqual({ q1: 42 });
        expect(withTyped({ q1: 'x' }, question('short'), '   ')).toEqual({});
    });

    it('says the answer key in words', () => {
        expect(describeKey('mcq', ['a', 'b'], [1])).toBe('B. b');
        expect(describeKey('numeric', null, { value: 0.75, tol: 0.01 })).toBe('0.75 (± 0.01)');
        expect(describeKey('short', null, { answers: ['inertia', 'Newton 1'], rubric: 'x' })).toBe(
            'inertia / Newton 1'
        );
    });
});
