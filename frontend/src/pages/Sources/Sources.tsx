import { type FormEvent, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { Icon } from '@/components/Icon/Icon';
import { Failed, Loading } from '@/components/Loading/States';
import { useApi } from '@/hooks/useApi';
import { api } from '@/lib/api';
import type { CourseMap, CourseSource, SourcesResponse } from '@/types/api';

/**
 * The learner's own course material: textbooks, slide decks and lecture videos.
 *
 * Three steps, top to bottom, because each one needs the one before it: add material, let it be
 * read, then turn it into a course map. Once a map exists the rest of the app — the tutor, practice,
 * the plan — works from this material instead of a generic syllabus.
 */

const KIND_LABEL: Record<CourseSource['kind'], string> = {
    pdf: 'Textbook / PDF',
    slides: 'Slides',
    video: 'Lecture video',
};

/** How often the list is refreshed while something is still being read. */
const POLL_MS = 4000;

export default function Sources() {
    const sources = useApi<SourcesResponse>(() => api.get<SourcesResponse>('/sources'));
    const organize = sources.data?.organize;

    const list = sources.data?.sources ?? [];
    const processing = list.some((source) => source.status === 'processing');
    const ready = list.filter((source) => source.status === 'ready');

    // Ingestion (and a course map waiting on it) runs on the server; poll until nothing is pending.
    useEffect(() => {
        if (!processing && organize?.status !== 'waiting') return;

        const timer = setInterval(sources.reload, POLL_MS);
        return () => clearInterval(timer);
    }, [processing, organize?.status, sources.reload]);

    const latestCurriculum = ready.find((source) => source.curriculumId)?.curriculumId ?? null;

    return (
        <div className="stack">
            <div className="row" style={{ gap: 14, alignItems: 'flex-start' }}>
                <span className="hero-mark" aria-hidden="true">
                    <Icon name="book" size={40} />
                </span>
                <div style={{ minWidth: 0 }}>
                    <span className="label">Course material</span>
                    <h1 style={{ marginBottom: 0 }}>Study from your own material</h1>
                    <p className="muted" style={{ margin: '6px 0 0' }}>
                        Add your textbook, slides and lecture videos. Adigam reads them, maps the
                        course, and answers with citations that open the exact page, slide or
                        moment.
                    </p>
                </div>
            </div>

            <AddMaterial onAdded={sources.reload} />

            {sources.loading && !sources.data && <Loading label="Loading your material…" />}
            {sources.error ? <Failed error={sources.error} onRetry={sources.reload} /> : null}

            {list.length > 0 && (
                <div className="card stack" style={{ gap: 8 }}>
                    <span className="label">Your material</span>
                    {list.map((source) => (
                        <SourceRow key={source.id} source={source} onChanged={sources.reload} />
                    ))}
                </div>
            )}

            {organize?.status === 'waiting' && (
                <div className="banner">
                    Your course map will be built as soon as all your material has been read.
                </div>
            )}
            {organize?.status === 'failed' && (
                <div className="banner banner--error">{organize.message}</div>
            )}

            {(ready.length > 0 || processing) && (
                <BuildCourse
                    key={latestCurriculum ?? 'none'}
                    disabled={organize?.status === 'waiting'}
                    initialCurriculumId={latestCurriculum}
                    onBuilt={sources.reload}
                />
            )}
        </div>
    );
}

export function AddMaterial({ onAdded }: { onAdded: () => void }) {
    const [mode, setMode] = useState<'file' | 'video'>('file');
    const [kind, setKind] = useState<'pdf' | 'slides'>('pdf');
    const [file, setFile] = useState<File | null>(null);
    const [videoUrl, setVideoUrl] = useState('');
    const [captions, setCaptions] = useState<File | null>(null);
    const [title, setTitle] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    async function submit(event: FormEvent) {
        event.preventDefault();
        if (busy) return;

        setError(null);
        setBusy(true);

        try {
            const name = title.trim() || file?.name.replace(/\.[^.]+$/, '') || 'Lecture';

            if (mode === 'file') {
                if (!file) throw new Error('Choose a file first.');

                await api.upload(
                    `/sources/upload?kind=${kind}&title=${encodeURIComponent(name)}`,
                    file,
                    'application/octet-stream'
                );
            } else if (captions) {
                await api.upload(
                    `/sources/video/captions?url=${encodeURIComponent(videoUrl.trim())}&title=${encodeURIComponent(name)}`,
                    await captions.text(),
                    'text/plain'
                );
            } else {
                await api.post('/sources/video', { url: videoUrl.trim(), title: name });
            }

            setFile(null);
            setCaptions(null);
            setVideoUrl('');
            setTitle('');
            onAdded();
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not add that material.');
        } finally {
            setBusy(false);
        }
    }

    return (
        <form className="card stack" onSubmit={submit}>
            <div className="row" style={{ gap: 8 }}>
                <button
                    type="button"
                    aria-pressed={mode === 'file'}
                    onClick={() => setMode('file')}
                >
                    Textbook or slides
                </button>
                <button
                    type="button"
                    aria-pressed={mode === 'video'}
                    onClick={() => setMode('video')}
                >
                    Lecture video
                </button>
            </div>

            {mode === 'file' ? (
                <>
                    <div className="field">
                        <label htmlFor="kind">What is it?</label>
                        <select
                            id="kind"
                            value={kind}
                            onChange={(e) => setKind(e.target.value as 'pdf' | 'slides')}
                        >
                            <option value="pdf">Textbook or notes (PDF)</option>
                            <option value="slides">Slide deck (.pptx or PDF)</option>
                        </select>
                    </div>
                    <div className="field">
                        <label htmlFor="file">File</label>
                        <input
                            id="file"
                            type="file"
                            accept={kind === 'pdf' ? '.pdf' : '.pdf,.pptx'}
                            onChange={(e) => setFile(e.target.files?.[0] ?? null)}
                        />
                        <span className="faint">
                            Up to 40 MB. Figures and diagrams are read too, for files up to 100
                            pages.
                        </span>
                    </div>
                </>
            ) : (
                <>
                    <div className="field">
                        <label htmlFor="video">YouTube link</label>
                        <input
                            id="video"
                            type="url"
                            value={videoUrl}
                            placeholder="https://www.youtube.com/watch?v=…"
                            onChange={(e) => setVideoUrl(e.target.value)}
                        />
                        <span className="faint">
                            The lecture's captions are used, with timestamps. NPTEL and MIT
                            OpenCourseWare lectures work well.
                        </span>
                    </div>
                    <div className="field">
                        <label htmlFor="captions">Captions file (optional)</label>
                        <input
                            id="captions"
                            type="file"
                            accept=".vtt,.srt"
                            onChange={(e) => setCaptions(e.target.files?.[0] ?? null)}
                        />
                        <span className="faint">
                            Only needed if the video has no captions on YouTube.
                        </span>
                    </div>
                </>
            )}

            <div className="field">
                <label htmlFor="title">Title</label>
                <input
                    id="title"
                    value={title}
                    maxLength={200}
                    placeholder={
                        mode === 'file' ? 'e.g. CLRS chapters 1–6' : 'e.g. Lecture 3: Sorting'
                    }
                    onChange={(e) => setTitle(e.target.value)}
                />
            </div>

            {error && <div className="banner banner--error">{error}</div>}

            <button
                type="submit"
                className="primary wide"
                disabled={busy || (mode === 'file' ? !file : videoUrl.trim() === '')}
            >
                {busy ? 'Adding…' : 'Add material'}
            </button>
        </form>
    );
}

function SourceRow({ source, onChanged }: { source: CourseSource; onChanged: () => void }) {
    const [removing, setRemoving] = useState(false);

    return (
        <div className="spread" style={{ gap: 12, padding: '6px 0' }}>
            <div style={{ minWidth: 0 }}>
                <strong>{source.title}</strong>
                <div className="faint">
                    {KIND_LABEL[source.kind]} · {source.status === 'processing' && 'Reading…'}
                    {source.status === 'ready' && `${source.excerptCount} excerpts`}
                    {source.status === 'failed' && (
                        <span style={{ color: 'var(--bad)' }}>Failed: {source.error}</span>
                    )}
                </div>
            </div>
            <button
                type="button"
                disabled={removing}
                onClick={async () => {
                    setRemoving(true);
                    try {
                        await api.delete(`/sources/${source.id}`);
                        onChanged();
                    } finally {
                        setRemoving(false);
                    }
                }}
            >
                Remove
            </button>
        </div>
    );
}

function BuildCourse({
    disabled,
    initialCurriculumId,
    onBuilt,
}: {
    disabled: boolean;
    initialCurriculumId: string | null;
    onBuilt: () => void;
}) {
    const [courseName, setCourseName] = useState('');
    const [building, setBuilding] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [map, setMap] = useState<CourseMap | null>(null);

    // Show the map that already exists, so revisiting the page does not mean rebuilding it.
    useEffect(() => {
        if (!initialCurriculumId || map) return;

        api.get<CourseMap>(`/sources/map/${initialCurriculumId}`)
            .then(setMap)
            .catch(() => undefined);
    }, [initialCurriculumId, map]);

    async function build(event: FormEvent) {
        event.preventDefault();
        if (building) return;

        setError(null);
        setBuilding(true);

        try {
            // With material still being read, the server builds the map when it is done.
            const result = await api.post<CourseMap | { status: 'waiting' }>('/sources/organize', {
                courseName: courseName.trim(),
                whenReady: true,
            });
            if (!('status' in result)) setMap(result);
            onBuilt();
        } catch (cause) {
            setError(cause instanceof Error ? cause.message : 'Could not build the course map.');
        } finally {
            setBuilding(false);
        }
    }

    return (
        <div className="stack">
            <form className="card stack" onSubmit={build}>
                <span className="label">
                    {map ? 'Rebuild the course map' : 'Build your course map'}
                </span>
                <p className="faint" style={{ margin: 0 }}>
                    Groups everything above into topics, key concepts and prerequisites. Practice,
                    mock tests and your study plan then come from this material.
                </p>
                <div className="field">
                    <label htmlFor="course">Course name</label>
                    <input
                        id="course"
                        value={courseName}
                        maxLength={120}
                        placeholder="e.g. Introduction to Algorithms"
                        onChange={(e) => setCourseName(e.target.value)}
                    />
                </div>
                {error && <div className="banner banner--error">{error}</div>}
                <button
                    type="submit"
                    className="primary wide"
                    disabled={disabled || building || courseName.trim().length < 2}
                >
                    {building ? 'Mapping the course… (about a minute)' : 'Build course map'}
                </button>
            </form>

            {map && <CourseMapView map={map} />}
        </div>
    );
}

/** Topics in teaching order, each with its concepts, prerequisites and how much material backs it. */
function CourseMapView({ map }: { map: CourseMap }) {
    return (
        <div className="card stack">
            <div className="spread">
                <span className="label">{map.curriculum?.name ?? 'Course map'}</span>
                {map.curriculum && (
                    <Link
                        className="primary"
                        to={`/goals?curriculum=${map.curriculum.id}&name=${encodeURIComponent(map.curriculum.name)}`}
                    >
                        Study this course →
                    </Link>
                )}
            </div>

            {map.subjects.map((subject) => (
                <div key={subject.id} className="stack" style={{ gap: 6 }}>
                    <strong>{subject.name}</strong>
                    <ol style={{ margin: 0, paddingLeft: 20 }}>
                        {subject.topics.map((topic) => (
                            <li key={topic.id} style={{ marginBottom: 8 }}>
                                <span>{topic.name}</span>
                                <span className="faint"> · {topic.excerptCount} excerpts</span>
                                {topic.keyConcepts.length > 0 && (
                                    <div className="faint">{topic.keyConcepts.join(' · ')}</div>
                                )}
                                {topic.prerequisites.length > 0 && (
                                    <div className="faint">
                                        Needs first:{' '}
                                        {topic.prerequisites.map((p) => p.name).join(', ')}
                                    </div>
                                )}
                            </li>
                        ))}
                    </ol>
                </div>
            ))}
        </div>
    );
}
