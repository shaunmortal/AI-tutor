import { type FormEvent, useEffect, useRef, useState } from 'react';
import { Citations } from '@/components/Citations/Citations';
import { Icon } from '@/components/Icon/Icon';
import { Link, useSearchParams } from 'react-router-dom';
import { Failed, Loading } from '@/components/Loading/States';
import { useApi } from '@/hooks/useApi';
import { useVoice } from '@/hooks/useVoice';
import { api } from '@/lib/api';
import type {
    CheckResult,
    ConversationSummary,
    QuickCheck,
    TutorMessage,
    TutorReply,
} from '@/types/api';

/** A message as shown here: a live reply may carry a quick check on what it explained. */
type ShownMessage = TutorMessage & { check?: QuickCheck };

/** Answers not taken from the learner's material start with this, in the server's own words. */
const GENERAL_KNOWLEDGE = 'General knowledge';

/**
 * The doubt-solving tutor.
 *
 * What makes it worth having over a general chatbot is the context the backend attaches: the
 * learner's mastery on the topic in view and the questions they recently got wrong. A learner at
 * mastery 20 gets the basic idea first; a learner at 80 does not get told what the topic is.
 *
 * Arriving from the practice screen passes `?topic=` and `?question=`, so "I don't understand
 * this" is answered about the question actually on screen instead of in the abstract.
 */
export default function AITutor() {
    const [params] = useSearchParams();
    const topicId = params.get('topic') ?? undefined;
    const aboutQuestion = params.get('question') ?? undefined;

    const history = useApi<{ conversations: ConversationSummary[] }>(() =>
        api.get<{ conversations: ConversationSummary[] }>('/ai/conversations')
    );

    const [conversationId, setConversationId] = useState<string | null>(null);
    const [messages, setMessages] = useState<ShownMessage[]>([]);
    const [question, setQuestion] = useState('');
    const [asking, setAsking] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const voice = useVoice();

    /**
     * Whether the answer should be read aloud.
     *
     * Only when the question was asked by voice. Speaking every answer would talk over somebody who
     * typed theirs in a library, and staying silent after somebody spoke theirs makes the feature
     * look half-finished. Asking how you asked is the answer to both.
     */
    const [spokenQuestion, setSpokenQuestion] = useState(false);

    // Keep the newest message in view as the exchange grows.
    const endRef = useRef<HTMLDivElement | null>(null);
    useEffect(() => {
        endRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' });
    }, [messages.length, asking]);

    async function ask(text: string) {
        const trimmed = text.trim();
        if (trimmed === '' || asking) return;

        setError(null);
        setAsking(true);
        voice.stopSpeaking();

        // Show the learner's own message straight away. Waiting for the round trip to echo it back
        // makes the app feel like it dropped what they typed.
        setMessages((current) => [
            ...current,
            { role: 'learner', content: trimmed, createdAt: new Date().toISOString() },
        ]);
        setQuestion('');

        try {
            const reply = await api.post<TutorReply>('/ai/tutor', {
                question: trimmed,
                ...(conversationId
                    ? { conversationId }
                    : {
                          // Only sent on the first message; afterwards the conversation carries
                          // its own topic, so a follow-up stays grounded without resending it.
                          ...(topicId ? { topicId } : {}),
                          ...(aboutQuestion ? { currentQuestion: aboutQuestion } : {}),
                      }),
            });

            setConversationId(reply.conversationId);
            setMessages((current) => [
                ...current,
                {
                    role: 'tutor',
                    content: reply.answer,
                    createdAt: new Date().toISOString(),
                    citations: reply.citations,
                    check: reply.check ?? undefined,
                },
            ]);

            if (spokenQuestion) voice.speak(reply.answer);

            // The thread now exists, or has moved to the top of the list.
            history.reload();
        } catch (cause) {
            setError(
                cause instanceof Error
                    ? cause.message
                    : 'Could not reach the tutor. Your question was saved — try again.'
            );
        } finally {
            setAsking(false);
        }
    }

    async function openThread(id: string) {
        setError(null);
        setConversationId(id);
        setMessages([]);

        try {
            const thread = await api.get<{ messages: TutorMessage[] }>(`/ai/conversations/${id}`);
            setMessages(thread.messages);
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not open that conversation.');
        }
    }

    function startNew() {
        setConversationId(null);
        setMessages([]);
        setError(null);
    }

    return (
        <div className="stack">
            <div className="spread">
                <div className="row" style={{ gap: 14, alignItems: 'flex-start' }}>
                    {/* The page's mark. Every screen has one so a page is recognisable at a glance
                    rather than being a heading above a stack of dark rectangles. Hidden on narrow
                    screens, where it would push the heading onto two awkward lines. */}
                    <span className="hero-mark" aria-hidden="true">
                        <Icon name="sparkles" size={40} />
                    </span>
                    <div style={{ minWidth: 0 }}>
                        <span className="label">Ask Adigam</span>
                        <h1 style={{ marginBottom: 0 }}>Stuck on something?</h1>
                    </div>
                </div>
                {messages.length > 0 && (
                    <button type="button" onClick={startNew}>
                        New question
                    </button>
                )}
            </div>

            {aboutQuestion && messages.length === 0 && (
                <div className="banner">
                    <span className="label">About this question</span>
                    <p style={{ margin: '6px 0 0' }}>{aboutQuestion}</p>
                </div>
            )}

            {messages.length === 0 && (
                <p className="muted" style={{ margin: 0 }}>
                    Ask anything about what you are studying. Answers use what you have already
                    practised, so they start where you actually are. With{' '}
                    <Link to="/sources">course material</Link> added, answers come from it, with
                    citations — and anything it does not cover is flagged as such.
                </p>
            )}

            {/* ------------------------------------------------ the exchange */}
            {messages.length > 0 && (
                <div className="stack" style={{ gap: 12 }}>
                    {messages.map((message, index) => (
                        <div
                            key={`${message.createdAt}-${index}`}
                            className={message.role === 'learner' ? 'card card--accent' : 'card'}
                        >
                            <div className="spread">
                                <span className="label">
                                    {message.role === 'learner' ? 'You' : 'Adigam'}
                                </span>
                                {message.role === 'tutor' && voice.canSpeak && (
                                    <button
                                        type="button"
                                        onClick={() =>
                                            voice.speaking
                                                ? voice.stopSpeaking()
                                                : voice.speak(message.content)
                                        }
                                    >
                                        {voice.speaking ? 'Stop' : '🔊 Listen'}
                                    </button>
                                )}
                            </div>
                            {/* Paragraphs preserved: the tutor is told to write short paragraphs
                                separated by blank lines, and collapsing them into one block would
                                undo the readability that was asked for. */}
                            {message.content
                                .split(/\n{2,}/)
                                .filter((part) => part.trim() !== '')
                                .map((paragraph, i) =>
                                    // Anything not from the learner's own material is set apart,
                                    // so it is never mistaken for what their course says.
                                    message.role === 'tutor' &&
                                    paragraph.startsWith(GENERAL_KNOWLEDGE) ? (
                                        <div
                                            key={i}
                                            className="banner"
                                            style={{ marginTop: i === 0 ? 8 : 12 }}
                                        >
                                            <span className="pill">General knowledge</span>
                                            <p
                                                style={{
                                                    margin: '8px 0 0',
                                                    whiteSpace: 'pre-wrap',
                                                }}
                                            >
                                                {paragraph}
                                            </p>
                                            {paragraph.includes('Sources page') && (
                                                <Link to="/sources">
                                                    Add your course material →
                                                </Link>
                                            )}
                                        </div>
                                    ) : (
                                        <p
                                            key={i}
                                            style={{
                                                marginTop: i === 0 ? 8 : 12,
                                                marginBottom: 0,
                                                whiteSpace: 'pre-wrap',
                                            }}
                                        >
                                            {paragraph}
                                        </p>
                                    )
                                )}
                            {message.role === 'tutor' && (
                                <Citations citations={message.citations ?? []} />
                            )}
                            {message.check && <QuickCheckCard check={message.check} />}
                        </div>
                    ))}

                    {asking && <Loading label="Thinking…" />}
                    <div ref={endRef} />
                </div>
            )}

            {error && <div className="banner banner--error">{error}</div>}

            {/* ------------------------------------------------ input */}
            <form
                className="card stack"
                onSubmit={(event: FormEvent) => {
                    event.preventDefault();
                    void ask(question);
                }}
            >
                <div className="field">
                    <label htmlFor="question">
                        {messages.length > 0 ? 'Ask a follow-up' : 'Your question'}
                    </label>
                    <input
                        id="question"
                        value={voice.listening ? voice.heard : question}
                        maxLength={2000}
                        placeholder={voice.listening ? 'Listening…' : 'Why does this formula work?'}
                        disabled={asking || voice.listening}
                        onChange={(e) => setQuestion(e.target.value)}
                    />
                    {voice.error && (
                        <span className="faint" style={{ color: 'var(--bad)' }}>
                            {voice.error}
                        </span>
                    )}
                </div>

                {/* The microphone is only rendered where the browser actually has speech
                    recognition — Firefox has none, and a button that silently does nothing is worse
                    than no button. Typing is always available. */}
                <div className="row" style={{ gap: 10 }}>
                    {voice.canListen && (
                        <button
                            type="button"
                            aria-pressed={voice.listening}
                            disabled={asking}
                            onClick={() => {
                                if (voice.listening) {
                                    voice.stopListening();
                                    return;
                                }

                                setSpokenQuestion(true);
                                // Sent as soon as the sentence finishes, so speaking a question is
                                // one action rather than speak-then-press-send.
                                voice.listen((said) => {
                                    setQuestion(said);
                                    void ask(said);
                                });
                            }}
                        >
                            {voice.listening ? 'Stop listening' : '🎙 Ask by voice'}
                        </button>
                    )}

                    <button
                        type="submit"
                        className="primary wide"
                        disabled={asking || voice.listening || question.trim() === ''}
                        onClick={() => setSpokenQuestion(false)}
                    >
                        {asking ? 'Asking…' : 'Ask'}
                    </button>
                </div>
            </form>

            {/* ------------------------------------------------ history */}
            {history.loading && <Loading label="Loading your questions…" />}
            {history.error ? <Failed error={history.error} onRetry={history.reload} /> : null}

            {(history.data?.conversations.length ?? 0) > 0 && (
                <div className="card stack" style={{ gap: 8 }}>
                    <span className="label">Your questions</span>

                    {history.data?.conversations.map((thread) => (
                        <button
                            key={thread.id}
                            type="button"
                            className="option"
                            aria-pressed={thread.id === conversationId}
                            onClick={() => void openThread(thread.id)}
                        >
                            <span className="option__mark">{thread.answered ? '' : '·'}</span>
                            <span>
                                {thread.title}
                                {!thread.answered && (
                                    <span className="faint"> — not answered yet</span>
                                )}
                            </span>
                        </button>
                    ))}
                </div>
            )}
        </div>
    );
}

/**
 * One question on what was just explained. Graded on the server and recorded like a practice
 * answer, so talking a doubt through also moves the learner's mastery and revision schedule.
 */
function QuickCheckCard({ check }: { check: QuickCheck }) {
    const [chosen, setChosen] = useState<number | null>(null);
    const [result, setResult] = useState<CheckResult | null>(null);
    const [error, setError] = useState<string | null>(null);

    async function answer(option: number) {
        if (chosen !== null) return;
        setChosen(option);
        setError(null);

        try {
            setResult(
                await api.post<CheckResult>(`/ai/checks/${check.questionId}/answer`, {
                    selectedOption: option,
                })
            );
        } catch (cause) {
            setChosen(null);
            setError(cause instanceof Error ? cause.message : 'Could not check that answer.');
        }
    }

    return (
        <div className="stack" style={{ gap: 8, marginTop: 14 }}>
            <span className="label">Quick check</span>
            <p style={{ margin: 0 }}>{check.body}</p>
            {check.options.map((option, index) => {
                const marking = result
                    ? result.correctAnswer.includes(index)
                        ? ' option--correct'
                        : index === chosen
                          ? ' option--wrong'
                          : ''
                    : '';

                return (
                    <button
                        key={index}
                        type="button"
                        className={`option${marking}`}
                        aria-pressed={index === chosen}
                        disabled={chosen !== null}
                        onClick={() => void answer(index)}
                    >
                        <span className="option__mark">{String.fromCharCode(65 + index)}</span>
                        <span>{option}</span>
                    </button>
                );
            })}
            {error && <div className="banner banner--error">{error}</div>}
            {result && (
                <div className={`banner banner--${result.isCorrect ? 'good' : 'error'}`}>
                    <strong>{result.isCorrect ? 'Right.' : 'Not quite.'}</strong>{' '}
                    {result.explanation}
                    <p className="faint" style={{ margin: '6px 0 0' }}>
                        Counted towards your mastery of this topic
                        {result.mastery.before === null
                            ? ` (now ${Math.round(result.mastery.after)}).`
                            : `: ${Math.round(result.mastery.before)} → ${Math.round(result.mastery.after)}.`}
                    </p>
                </div>
            )}
            {result && <Citations citations={[check.citation]} />}
        </div>
    );
}
