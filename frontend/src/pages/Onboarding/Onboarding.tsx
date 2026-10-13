import { type FormEvent, useEffect, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { Icon } from '@/components/Icon/Icon';
import { Failed, Loading } from '@/components/Loading/States';
import { useApi } from '@/hooks/useApi';
import { api } from '@/lib/api';
import { AddMaterial } from '@/pages/Sources/Sources';
import type {
    CourseMap,
    DiagnosticList,
    Goal,
    PreferredLanguage,
    ResolveOutcome,
    SourcesResponse,
} from '@/types/api';

/**
 * First run: from "who are you studying for" to a plan, in one place.
 *
 * About you → your material (the course map builds itself once it has been read) → how sure you
 * are of each area → a short knowledge check → the plan. Self-rated confidence is only a starting
 * guess for topics with no answers yet; the first real answer replaces it.
 *
 * Where the learner is comes from the server (a goal exists, the check was taken), so leaving
 * half-way and coming back resumes rather than restarts.
 */

const STEPS = ['About you', 'Your material', 'Confidence', 'Knowledge check', 'Your plan'];

const LANGUAGES: { value: PreferredLanguage; label: string }[] = [
    { value: 'english', label: 'English' },
    { value: 'hindi', label: 'हिन्दी' },
    { value: 'hinglish', label: 'Hinglish' },
];

const CONFIDENCE = ['Not at all', 'A little', 'Somewhat', 'Fairly', 'Very'];

/** Remembered on this device only: a skipped check is a choice, not a server fact. */
const SKIP_KEY = 'adigam.onboarding.skipCheck';

const POLL_MS = 4000;

interface Intake {
    course: string;
    examDate: string;
    dailyMinutes: number;
}

interface Area {
    id: string;
    name: string;
}

function isoDaysFromNow(days: number): string {
    const d = new Date();
    d.setDate(d.getDate() + days);
    return d.toISOString().slice(0, 10);
}

function readSkip(): boolean {
    try {
        return localStorage.getItem(SKIP_KEY) === '1';
    } catch {
        return false;
    }
}

export default function Onboarding() {
    const goal = useApi<{ goal: Goal | null }>(() => api.get<{ goal: Goal | null }>('/goals'));
    const hasGoal = Boolean(goal.data?.goal);
    const diagnostic = useApi<DiagnosticList | null>(
        () =>
            hasGoal ? api.get<DiagnosticList>('/assessments/diagnostic') : Promise.resolve(null),
        [hasGoal]
    );

    const [setupStep, setSetupStep] = useState(0);
    const [skipped, setSkipped] = useState(readSkip);

    const checked = Boolean(diagnostic.data?.assessment?.completedAt) || skipped;
    const step = !hasGoal ? setupStep : checked ? 4 : 3;

    return (
        <div className="stack">
            <div className="row" style={{ gap: 14, alignItems: 'flex-start' }}>
                <span className="hero-mark" aria-hidden="true">
                    <Icon name="target" size={40} />
                </span>
                <div style={{ minWidth: 0 }}>
                    <span className="label">Getting started</span>
                    <h1 style={{ marginBottom: 0 }}>Let us set up your course</h1>
                    <p className="muted" style={{ margin: '6px 0 0' }}>
                        A few minutes now, and every session after this starts where you actually
                        are.
                    </p>
                </div>
            </div>

            <ol className="steps steps--progress">
                {STEPS.map((name, i) => (
                    <li
                        key={name}
                        className="step"
                        aria-current={i === step ? 'step' : undefined}
                        style={{ opacity: i === step ? 1 : 0.55 }}
                    >
                        <span className="step__n">{i < step ? '✓' : i + 1}</span>
                        <span>{name}</span>
                    </li>
                ))}
            </ol>

            {goal.loading && <Loading />}
            {goal.error ? <Failed error={goal.error} onRetry={goal.reload} /> : null}

            {goal.data && !hasGoal && (
                <Setup step={setupStep} onStep={setSetupStep} onDone={goal.reload} />
            )}

            {hasGoal && step === 3 && (
                <div className="card stack">
                    <span className="label">A short knowledge check</span>
                    <p style={{ margin: 0 }}>
                        A few questions across your course replace your own guesses with real
                        evidence, so the plan spends time where you are actually weak. When you
                        finish, come back here for your plan.
                    </p>
                    <div className="row" style={{ gap: 10 }}>
                        <button
                            type="button"
                            onClick={() => {
                                try {
                                    localStorage.setItem(SKIP_KEY, '1');
                                } catch {
                                    // Private mode: the skip lasts for this visit only.
                                }
                                setSkipped(true);
                            }}
                        >
                            Skip for now
                        </button>
                        <Link
                            className="primary btn"
                            style={{ flex: '1 1 220px' }}
                            to="/assessment"
                        >
                            Start the knowledge check
                        </Link>
                    </div>
                </div>
            )}

            {hasGoal && step === 4 && <BuildPlan />}
        </div>
    );
}

function Setup({
    step,
    onStep,
    onDone,
}: {
    step: number;
    onStep: (step: number) => void;
    onDone: () => void;
}) {
    const [intake, setIntake] = useState<Intake>({
        course: '',
        examDate: isoDaysFromNow(60),
        dailyMinutes: 60,
    });
    const [course, setCourse] = useState<{ id: string; name: string; areas: Area[] } | null>(null);

    if (step === 0) {
        return (
            <AboutYou
                intake={intake}
                onSubmit={(next) => {
                    setIntake(next);
                    onStep(1);
                }}
            />
        );
    }

    if (step === 1 || !course) {
        return (
            <Material
                courseName={intake.course}
                onBack={() => onStep(0)}
                onMapped={(mapped) => {
                    setCourse(mapped);
                    onStep(2);
                }}
            />
        );
    }

    return <Confidence intake={intake} course={course} onDone={onDone} />;
}

function AboutYou({ intake, onSubmit }: { intake: Intake; onSubmit: (next: Intake) => void }) {
    const [form, setForm] = useState(intake);
    const [language, setLanguage] = useState<PreferredLanguage>('english');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    async function submit(event: FormEvent) {
        event.preventDefault();
        setBusy(true);
        setError(null);

        try {
            await api.patch('/learner/profile', { preferredLanguage: language });
            onSubmit({ ...form, course: form.course.trim() });
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not save that.');
        } finally {
            setBusy(false);
        }
    }

    return (
        <form className="card stack" onSubmit={submit}>
            <div className="field">
                <label htmlFor="course">What are you studying for?</label>
                <input
                    id="course"
                    value={form.course}
                    maxLength={120}
                    required
                    placeholder="e.g. GATE CSE, Class 12 Physics, DBMS end-sem"
                    onChange={(e) => setForm({ ...form, course: e.target.value })}
                />
            </div>
            <div className="field">
                <label htmlFor="examDate">When is the exam?</label>
                <input
                    id="examDate"
                    type="date"
                    value={form.examDate}
                    min={isoDaysFromNow(1)}
                    required
                    onChange={(e) => setForm({ ...form, examDate: e.target.value })}
                />
            </div>
            <div className="field">
                <label htmlFor="minutes">Minutes a day you can study</label>
                <input
                    id="minutes"
                    type="number"
                    min={10}
                    max={960}
                    step={5}
                    value={form.dailyMinutes}
                    required
                    onChange={(e) => setForm({ ...form, dailyMinutes: Number(e.target.value) })}
                />
            </div>
            <div className="field">
                <span id="language-label">Explain things to me in</span>
                <div
                    className="row"
                    style={{ gap: 8 }}
                    role="group"
                    aria-labelledby="language-label"
                >
                    {LANGUAGES.map((l) => (
                        <button
                            key={l.value}
                            type="button"
                            aria-pressed={language === l.value}
                            onClick={() => setLanguage(l.value)}
                        >
                            {l.label}
                        </button>
                    ))}
                </div>
            </div>
            {error && <div className="banner banner--error">{error}</div>}
            <button
                type="submit"
                className="primary wide"
                disabled={busy || form.course.trim().length < 2}
            >
                Next
            </button>
        </form>
    );
}

/**
 * Upload, then one press: the server builds the course map as soon as everything has been read,
 * even if this tab is closed meanwhile. No material yet falls back to a standard syllabus.
 */
function Material({
    courseName,
    onBack,
    onMapped,
}: {
    courseName: string;
    onBack: () => void;
    onMapped: (course: { id: string; name: string; areas: Area[] }) => void;
}) {
    const sources = useApi<SourcesResponse>(() => api.get<SourcesResponse>('/sources'));
    const [waiting, setWaiting] = useState(false);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const list = sources.data?.sources ?? [];
    const processing = list.some((s) => s.status === 'processing');
    const organize = sources.data?.organize.status ?? 'idle';

    useEffect(() => {
        if (!processing && !(waiting && organize === 'waiting')) return;
        const timer = setInterval(sources.reload, POLL_MS);
        return () => clearInterval(timer);
    }, [processing, waiting, organize, sources.reload]);

    // The map was being built in the background: pick it up once it exists.
    useEffect(() => {
        if (!waiting || organize === 'waiting') return;
        if (organize === 'failed') {
            setWaiting(false);
            setError(sources.data?.organize.message ?? 'Could not build the course map.');
            return;
        }
        const curriculumId = list.find((s) => s.curriculumId)?.curriculumId;
        if (!curriculumId) return;
        setWaiting(false);
        api.get<CourseMap>(`/sources/map/${curriculumId}`)
            .then(fromMap)
            .catch((cause) => setError(cause instanceof Error ? cause.message : String(cause)));
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [waiting, organize, list]);

    function fromMap(map: CourseMap) {
        if (!map.curriculum) throw new Error('The course map has no course.');
        onMapped({
            id: map.curriculum.id,
            name: map.curriculum.name,
            areas: map.subjects.map(({ id, name }) => ({ id, name })),
        });
    }

    async function build() {
        setBusy(true);
        setError(null);
        try {
            const result = await api.post<CourseMap | { status: 'waiting' }>('/sources/organize', {
                courseName,
                whenReady: true,
            });
            if ('status' in result) {
                setWaiting(true);
                sources.reload();
            } else {
                fromMap(result);
            }
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not build the course map.');
        } finally {
            setBusy(false);
        }
    }

    async function fromSyllabus() {
        setBusy(true);
        setError(null);
        try {
            const outcome = await api.post<ResolveOutcome>('/curricula/resolve', {
                goalText: courseName,
            });
            if (outcome.status === 'rejected' || !outcome.curriculum) {
                throw new Error(outcome.message ?? 'That does not look like a course.');
            }
            onMapped({
                id: outcome.curriculum.id,
                name: outcome.curriculum.name,
                areas: (outcome.subjects ?? []).map(({ id, name }) => ({ id, name })),
            });
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not find a syllabus.');
        } finally {
            setBusy(false);
        }
    }

    return (
        <div className="stack">
            <p className="muted" style={{ margin: 0 }}>
                Add your textbook, notes, slides or lecture videos for {courseName}. Answers,
                questions and flashcards will then come from them, with citations.
            </p>
            <AddMaterial onAdded={sources.reload} />

            {list.length > 0 && (
                <div className="card stack" style={{ gap: 6 }}>
                    <span className="label">Your material</span>
                    {list.map((s) => (
                        <div key={s.id} className="spread">
                            <span>{s.title}</span>
                            <span className="faint">
                                {s.status === 'processing'
                                    ? 'Reading…'
                                    : s.status === 'ready'
                                      ? `${s.excerptCount} excerpts`
                                      : 'Failed'}
                            </span>
                        </div>
                    ))}
                </div>
            )}

            {error && <div className="banner banner--error">{error}</div>}

            {waiting ? (
                <Loading
                    label={
                        processing
                            ? 'Reading your material. The course map is built as soon as it is done — you can leave this page.'
                            : 'Building your course map…'
                    }
                />
            ) : (
                <div className="stack" style={{ gap: 10 }}>
                    <button
                        type="button"
                        className="primary wide"
                        disabled={busy || list.length === 0}
                        onClick={() => void build()}
                    >
                        {busy ? 'Working…' : 'Build my course map'}
                    </button>
                    <div className="row" style={{ gap: 10 }}>
                        <button type="button" onClick={onBack} disabled={busy}>
                            Back
                        </button>
                        <button
                            type="button"
                            className="linkish"
                            disabled={busy}
                            onClick={() => void fromSyllabus()}
                        >
                            No material yet? Use a standard syllabus
                        </button>
                    </div>
                </div>
            )}
        </div>
    );
}

function Confidence({
    intake,
    course,
    onDone,
}: {
    intake: Intake;
    course: { id: string; name: string; areas: Area[] };
    onDone: () => void;
}) {
    const areas = course.areas.slice(0, 20); // a goal covers at most 20 subjects
    const [levels, setLevels] = useState<Record<string, number>>({});
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    async function submit() {
        setBusy(true);
        setError(null);
        try {
            await api.put('/learner/confidence', {
                ratings: areas.map((a) => ({ topicId: a.id, confidence: levels[a.id] ?? 3 })),
            });
            await api.post('/goals', {
                title: intake.course.slice(0, 120),
                curriculumId: course.id,
                examDate: intake.examDate,
                dailyMinutes: intake.dailyMinutes,
                subjectIds: areas.map((a) => a.id),
            });
            onDone();
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not save your goal.');
        } finally {
            setBusy(false);
        }
    }

    return (
        <div className="card stack">
            <span className="label">{course.name}: how sure are you of each area?</span>
            <p className="faint" style={{ margin: 0 }}>
                A starting guess only. It decides where the plan begins; your first answers in each
                area replace it.
            </p>
            {areas.map((area) => (
                <div key={area.id} className="stack" style={{ gap: 6 }}>
                    <span id={`area-${area.id}`}>{area.name}</span>
                    <div
                        className="row"
                        style={{ gap: 6, flexWrap: 'wrap' }}
                        role="group"
                        aria-labelledby={`area-${area.id}`}
                    >
                        {CONFIDENCE.map((label, i) => (
                            <button
                                key={label}
                                type="button"
                                aria-pressed={(levels[area.id] ?? 3) === i + 1}
                                onClick={() => setLevels({ ...levels, [area.id]: i + 1 })}
                            >
                                {label}
                            </button>
                        ))}
                    </div>
                </div>
            ))}
            {error && <div className="banner banner--error">{error}</div>}
            <button
                type="button"
                className="primary wide"
                disabled={busy || areas.length === 0}
                onClick={() => void submit()}
            >
                {busy ? 'Saving…' : 'Save and continue'}
            </button>
        </div>
    );
}

function BuildPlan() {
    const navigate = useNavigate();
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    async function build() {
        setBusy(true);
        setError(null);
        try {
            await api.post('/study-plan/generate', {});
            navigate('/plan');
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not build the plan.');
            setBusy(false);
        }
    }

    return (
        <div className="card stack">
            <span className="label">Your plan</span>
            <p style={{ margin: 0 }}>
                Day by day until your exam: weakest areas first, and nothing before the topics it
                builds on.
            </p>
            {error && <div className="banner banner--error">{error}</div>}
            <button
                type="button"
                className="primary wide"
                disabled={busy}
                onClick={() => void build()}
            >
                {busy ? 'Building…' : 'Build my study plan'}
            </button>
        </div>
    );
}
