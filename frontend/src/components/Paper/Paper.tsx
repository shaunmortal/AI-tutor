import { Citations } from '@/components/Citations/Citations';
import type { MockReviewItem, PracticeQuestion } from '@/types/api';
import { describeKey } from './answers';

/**
 * What the diagnostic, mock tests and practice share about answering and reviewing a question.
 *
 * Choice questions are answered by tapping options; numeric and short-answer questions by typing.
 * A typed short answer is marked on the server — a normalised match against the accepted wordings,
 * and only then against a rubric — so the browser never holds an answer key to compare with.
 */

/** The answer box for numeric and short-answer questions. */
export function TypedAnswer({
    question,
    value,
    disabled,
    onChange,
}: {
    question: PracticeQuestion;
    value: string;
    disabled?: boolean;
    onChange: (value: string) => void;
}) {
    const id = `answer-${question.id}`;
    const short = question.question_type === 'short';

    return (
        <div className="field">
            <label htmlFor={id}>Your answer</label>
            <input
                id={id}
                type={short ? 'text' : 'number'}
                step={short ? undefined : 'any'}
                inputMode={short ? 'text' : 'decimal'}
                maxLength={short ? 2000 : undefined}
                autoComplete="off"
                value={value}
                disabled={disabled}
                onChange={(e) => onChange(e.target.value)}
            />
            {short && (
                <span className="faint">
                    A word or a short phrase is enough. Spelling and wording need not be exact.
                </span>
            )}
        </div>
    );
}

const statusLabel: Record<MockReviewItem['status'], string> = {
    correct: 'Correct',
    wrong: 'Wrong',
    skipped: 'Skipped',
    unmarked: 'Not marked',
};

/**
 * Every question of a marked paper: what was given, what was right, why, and the page, slide or
 * moment of the learner's own material it came from.
 */
export function ReviewList({ items }: { items: MockReviewItem[] }) {
    return (
        <div className="stack" style={{ gap: 12 }}>
            <span className="label">Question by question</span>

            {items.map((item, n) => {
                const chosen = item.givenAnswer?.selectedOptions ?? [];
                const key = Array.isArray(item.correctAnswer)
                    ? (item.correctAnswer as number[])
                    : [];

                return (
                    <div key={item.questionId} className="card stack" style={{ gap: 10 }}>
                        <div className="spread" style={{ gap: 10 }}>
                            <span className="faint">
                                {n + 1}. {item.topicName}
                            </span>
                            <span
                                className={`pill ${item.status === 'correct' ? 'pill--easy' : item.status === 'wrong' ? 'pill--hard' : ''}`}
                            >
                                {statusLabel[item.status]}
                            </span>
                        </div>

                        <p style={{ margin: 0 }}>{item.body}</p>

                        {item.options ? (
                            <div className="stack" style={{ gap: 6 }}>
                                {item.options.map((option, i) => (
                                    <div
                                        key={i}
                                        className={`option${key.includes(i) ? ' option--correct' : chosen.includes(i) ? ' option--wrong' : ''}`}
                                        aria-label={
                                            key.includes(i)
                                                ? 'correct option'
                                                : chosen.includes(i)
                                                  ? 'your option'
                                                  : undefined
                                        }
                                    >
                                        <span className="option__mark">
                                            {String.fromCharCode(65 + i)}
                                        </span>
                                        <span>{option}</span>
                                    </div>
                                ))}
                            </div>
                        ) : (
                            <div className="stack" style={{ gap: 4 }}>
                                <span>
                                    <span className="faint">Your answer: </span>
                                    {item.givenAnswer
                                        ? String(item.givenAnswer.text ?? item.givenAnswer.value)
                                        : '—'}
                                </span>
                                <span>
                                    <span className="faint">Answer: </span>
                                    {describeKey(
                                        item.questionType,
                                        item.options,
                                        item.correctAnswer
                                    )}
                                </span>
                            </div>
                        )}

                        {item.status === 'unmarked' && (
                            <p className="faint" style={{ margin: 0 }}>
                                This question could not be marked, so it is left out of your score.
                            </p>
                        )}
                        {item.feedback && <p style={{ margin: 0 }}>{item.feedback}</p>}
                        {item.misconception && (
                            <div className="banner banner--warn">
                                <strong>Likely misconception:</strong> {item.misconception}
                            </div>
                        )}
                        {item.explanation && (
                            <div>
                                <span className="label">Why</span>
                                <p style={{ margin: '6px 0 0' }}>{item.explanation}</p>
                            </div>
                        )}
                        {item.source && <Citations citations={[item.source]} />}
                    </div>
                );
            })}
        </div>
    );
}
