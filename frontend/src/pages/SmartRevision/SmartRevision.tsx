import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { Citations } from '@/components/Citations/Citations';
import { Icon } from '@/components/Icon/Icon';
import { Empty, Failed, Loading } from '@/components/Loading/States';
import { useApi } from '@/hooks/useApi';
import { toUtterances } from '@/hooks/useVoice';
import { api } from '@/lib/api';
import type {
    DueRevision,
    FlashcardDeck,
    FlashcardTopic,
    LearnerProfile,
    PreferredLanguage,
} from '@/types/api';
import { briefParts, pickVoice } from './brief';

/**
 * Revision: what the schedule says is due, and flashcards for it.
 *
 * Cards are written once per topic from the learner's own excerpts, so every card cites the page,
 * slide or moment it came from. "Known" and "Again" move the same spaced-repetition schedule that
 * practice moves: a card is evidence, not a game score.
 */
export default function SmartRevision() {
    const due = useApi<{ today: string; due: DueRevision[] }>(() =>
        api.get<{ today: string; due: DueRevision[] }>('/revision/due')
    );
    const deck = useApi<FlashcardDeck>(() => api.get<FlashcardDeck>('/revision/flashcards'));
    const profile = useApi<{ profile: LearnerProfile }>(() =>
        api.get<{ profile: LearnerProfile }>('/learner/profile')
    );

    return (
        <div className="stack">
            <div className="row" style={{ gap: 14, alignItems: 'flex-start' }}>
                <span className="hero-mark" aria-hidden="true">
                    <Icon name="flame" size={40} />
                </span>
                <div style={{ minWidth: 0 }}>
                    <span className="label">Revision</span>
                    <h1 style={{ marginBottom: 0 }}>Keep what you have learned</h1>
                    <p className="muted" style={{ margin: '6px 0 0' }}>
                        Topics come back just before you would forget them. Flip a card, then say
                        honestly whether you knew it.
                    </p>
                </div>
            </div>

            {/* ------------------------------------------------ due */}
            {due.loading && <Loading label="Checking what is due…" />}
            {due.error ? <Failed error={due.error} onRetry={due.reload} /> : null}
            {due.data && due.data.due.length > 0 && (
                <div className="card stack" style={{ gap: 8 }}>
                    <span className="label">Due for revision</span>
                    {due.data.due.map((topic) => (
                        <div
                            key={topic.topicId}
                            className="spread"
                            style={{ flexWrap: 'wrap', alignItems: 'baseline' }}
                        >
                            <span>
                                {topic.name}
                                {topic.subjectName && (
                                    <span className="faint"> · {topic.subjectName}</span>
                                )}
                            </span>
                            <span className="faint">
                                {topic.daysOverdue === 0
                                    ? 'due today'
                                    : `${topic.daysOverdue} ${topic.daysOverdue === 1 ? 'day' : 'days'} overdue`}
                            </span>
                        </div>
                    ))}
                    <Link to="/practice">Practise these →</Link>
                </div>
            )}

            {/* ------------------------------------------------ cards */}
            {deck.loading && (
                <Loading
                    label="Getting your flashcards…"
                    slowLabel="Writing flashcards from your material — this takes a few seconds the first time."
                />
            )}
            {deck.error ? <Failed error={deck.error} onRetry={deck.reload} /> : null}
            {deck.data && deck.data.topics.length === 0 && (
                <Empty
                    title="No flashcards yet"
                    action={<Link to="/sources">Add course material</Link>}
                >
                    <p className="muted">
                        Cards are made from your own material for topics that are due or weak. Once
                        you have practised a little on a course built from your material, they
                        appear here.
                    </p>
                </Empty>
            )}
            {deck.data?.incomplete && (
                <p className="faint" style={{ margin: 0 }}>
                    Cards for some topics could not be written just now. They will be next time.
                </p>
            )}
            {deck.data && deck.data.topics.length > 0 && (
                <AudioBrief
                    topics={deck.data.topics}
                    language={profile.data?.profile.preferredLanguage ?? 'english'}
                />
            )}
            {deck.data?.topics.map((topic) => (
                <TopicDeck key={topic.topicId} topic={topic} />
            ))}
        </div>
    );
}

function TopicDeck({ topic }: { topic: FlashcardTopic }) {
    const [index, setIndex] = useState(0);
    const [flipped, setFlipped] = useState(false);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const done = index >= topic.cards.length;
    const card = topic.cards[Math.min(index, topic.cards.length - 1)];

    async function mark(known: boolean) {
        setBusy(true);
        setError(null);

        try {
            await api.post(`/revision/flashcards/${card.id}/review`, { known });
            setFlipped(false);
            setIndex((i) => i + 1);
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not save that.');
        } finally {
            setBusy(false);
        }
    }

    return (
        <div className="stack" style={{ gap: 10 }}>
            <div className="spread">
                <span className="label">{topic.name}</span>
                <span className="faint">
                    {topic.reason === 'due' ? 'due' : 'weak'} ·{' '}
                    {done ? 'done' : `${index + 1} of ${topic.cards.length}`}
                </span>
            </div>

            {done ? (
                <div className="banner banner--good">
                    That is this topic for today.{' '}
                    <button type="button" className="linkish" onClick={() => setIndex(0)}>
                        Go through it again
                    </button>
                </div>
            ) : (
                <>
                    <div className="flipcard" data-flipped={flipped}>
                        <div className="flipcard__inner">
                            <div
                                className="card flipcard__face stack"
                                aria-hidden={flipped}
                                inert={flipped}
                            >
                                <span className="faint">Question</span>
                                <p style={{ margin: 0, fontSize: '1.1rem' }}>{card.front}</p>
                            </div>
                            <div
                                className="card card--accent flipcard__face flipcard__face--back stack"
                                aria-hidden={!flipped}
                                inert={!flipped}
                            >
                                <span className="faint">Answer</span>
                                <p style={{ margin: 0 }}>{card.back}</p>
                                <Citations citations={card.citations} />
                            </div>
                        </div>
                    </div>

                    {error && <div className="banner banner--error">{error}</div>}

                    {flipped ? (
                        <div className="row" style={{ gap: 10 }}>
                            <button type="button" disabled={busy} onClick={() => void mark(false)}>
                                Again
                            </button>
                            <button
                                type="button"
                                className="primary wide"
                                disabled={busy}
                                onClick={() => void mark(true)}
                            >
                                I knew it
                            </button>
                        </div>
                    ) : (
                        <button
                            type="button"
                            className="primary wide"
                            onClick={() => setFlipped(true)}
                        >
                            Show answer
                        </button>
                    )}
                </>
            )}
        </div>
    );
}

/* ------------------------------------------------------------------ audio brief */

/**
 * "Listen": the due and weak topics and their cards read aloud by the browser's own voice, with
 * the card being read highlighted. Nothing goes to the server, and nothing plays until asked.
 */
function AudioBrief({
    topics,
    language,
}: {
    topics: FlashcardTopic[];
    language: PreferredLanguage;
}) {
    const canSpeak = typeof window !== 'undefined' && 'speechSynthesis' in window;
    const [state, setState] = useState<'idle' | 'playing' | 'paused'>('idle');
    const [current, setCurrent] = useState<string | null>(null);
    // Bumped on every stop, so events from a cancelled run cannot move the highlight.
    const run = useRef(0);

    // Leaving the page stops the voice; it is not tied to the React tree.
    useEffect(
        () => () => {
            if (canSpeak) window.speechSynthesis.cancel();
        },
        [canSpeak]
    );

    if (!canSpeak) return null;

    function stop() {
        run.current += 1;
        window.speechSynthesis.cancel();
        setState('idle');
        setCurrent(null);
    }

    function play() {
        const synth = window.speechSynthesis;
        stop();
        const mine = run.current;

        const voices = synth.getVoices();
        const { lang, index } = pickVoice(voices, language);
        const parts = briefParts(topics, lang === 'hi-IN');
        const chunks = parts.flatMap((part) =>
            toUtterances(part.text).map((text) => ({ text, cardId: part.cardId }))
        );

        chunks.forEach((chunk, i) => {
            const utterance = new SpeechSynthesisUtterance(chunk.text);
            utterance.lang = lang;
            if (index >= 0) utterance.voice = voices[index]!;
            utterance.onstart = () => {
                if (run.current === mine) setCurrent(chunk.cardId);
            };
            // A refusal (no user gesture, no audio device) must not leave the controls stuck on
            // "playing" over silence. Interruptions are our own stop, already handled.
            utterance.onerror = (event) => {
                if (run.current !== mine) return;
                if (event.error !== 'interrupted' && event.error !== 'canceled') stop();
            };
            if (i === chunks.length - 1) {
                utterance.onend = () => {
                    if (run.current === mine) stop();
                };
            }
            synth.speak(utterance);
        });
        setState('playing');
    }

    const cards = topics.flatMap((topic) =>
        topic.cards.map((card) => ({ ...card, topicName: topic.name }))
    );

    return (
        <div className="card stack brief" data-playing={state !== 'idle'} style={{ gap: 10 }}>
            <div className="spread" style={{ flexWrap: 'wrap' }}>
                <div style={{ minWidth: 0 }}>
                    <span className="label">Audio brief</span>
                    <div className="faint">
                        {cards.length} {cards.length === 1 ? 'card' : 'cards'} read aloud
                        {language === 'hindi' ? ' — in Hindi where your device has a voice' : ''}.
                    </div>
                </div>
                <div className="row" style={{ gap: 8 }}>
                    {state === 'idle' ? (
                        <button type="button" className="primary" onClick={play}>
                            ▶ Listen
                        </button>
                    ) : (
                        <>
                            <button
                                type="button"
                                aria-pressed={state === 'paused'}
                                onClick={() => {
                                    if (state === 'paused') {
                                        window.speechSynthesis.resume();
                                        setState('playing');
                                    } else {
                                        window.speechSynthesis.pause();
                                        setState('paused');
                                    }
                                }}
                            >
                                {state === 'paused' ? '▶ Resume' : '❚❚ Pause'}
                            </button>
                            <button type="button" onClick={stop}>
                                ■ Stop
                            </button>
                        </>
                    )}
                </div>
            </div>

            {state !== 'idle' && (
                <ol className="stack" style={{ gap: 4, margin: 0, padding: 0, listStyle: 'none' }}>
                    {cards.map((card) => (
                        <li
                            key={card.id}
                            className="brief__item"
                            aria-current={card.id === current ? 'true' : undefined}
                        >
                            <span className="faint">{card.topicName} · </span>
                            {card.front}
                        </li>
                    ))}
                </ol>
            )}
        </div>
    );
}
