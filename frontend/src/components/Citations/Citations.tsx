import { useEffect, useState } from 'react';
import { api } from '@/lib/api';
import type { Citation, OpenedExcerpt } from '@/types/api';
import { isDeck } from './deck';

/**
 * Where an answer came from: one chip per cited excerpt.
 *
 * Clicking a chip shows the excerpt and a link to the source at that spot — the PDF at its page,
 * the video at its second. A .pptx has no viewer that can open at a slide, so a slide excerpt says
 * which slide and offers the deck to download instead.
 */
export function Citations({ citations }: { citations: Citation[] }) {
    const [shown, setShown] = useState<string | null>(null);

    if (citations.length === 0) return null;

    return (
        <div className="stack" style={{ gap: 6, marginTop: 10 }}>
            <span className="label">Sources</span>
            <div className="row" style={{ gap: 6, flexWrap: 'wrap' }}>
                {citations.map((citation) => (
                    <button
                        key={citation.chunkId}
                        type="button"
                        title={citation.excerpt}
                        onClick={() =>
                            setShown(shown === citation.chunkId ? null : citation.chunkId)
                        }
                        aria-expanded={shown === citation.chunkId}
                    >
                        [{citation.n}] {citation.sourceTitle} · {citation.location}
                        {citation.modality === 'figure' ? ' · figure' : ''}
                    </button>
                ))}
            </div>
            {citations
                .filter((citation) => citation.chunkId === shown)
                .map((citation) => (
                    <div key={citation.chunkId} className="banner">
                        <p style={{ margin: 0, whiteSpace: 'pre-wrap' }}>{citation.excerpt}</p>
                        <SourceLink chunkId={citation.chunkId} />
                    </div>
                ))}
        </div>
    );
}

/** "Slide N" and a deck download, for an excerpt that cannot be opened at its spot. */
function DeckLink({ opened }: { opened: OpenedExcerpt }) {
    return (
        <span className="row" style={{ gap: 10, marginTop: 8 }}>
            <span className="pill">Slide {opened.slide}</span>
            <a
                className="btn btn-link"
                href={opened.url ?? undefined}
                download
                style={{ padding: '8px 14px' }}
            >
                Download deck
            </a>
            <span className="faint">
                Slide decks open from the start — go to slide {opened.slide}.
            </span>
        </span>
    );
}

/**
 * The link for one expanded citation, looked up as soon as the excerpt is shown — so the link is a
 * real link (no pop-up dance) and a slide says "Slide N" before anything is clicked.
 */
function SourceLink({ chunkId }: { chunkId: string }) {
    const [opened, setOpened] = useState<OpenedExcerpt | null>(null);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        let live = true;
        api.get<OpenedExcerpt>(`/sources/excerpts/${chunkId}/open`)
            .then((result) => live && setOpened(result))
            .catch(
                (cause) =>
                    live &&
                    setError(cause instanceof Error ? cause.message : 'Could not find the source.')
            );
        return () => {
            live = false;
        };
    }, [chunkId]);

    if (error)
        return (
            <p className="faint" style={{ margin: '8px 0 0' }}>
                {error}
            </p>
        );
    if (!opened)
        return (
            <p className="faint" style={{ margin: '8px 0 0' }}>
                Finding the source…
            </p>
        );
    if (isDeck(opened)) return <DeckLink opened={opened} />;
    if (!opened.url) return null;

    return (
        <span className="row" style={{ gap: 10, marginTop: 8 }}>
            {opened.slide !== null && <span className="pill">Slide {opened.slide}</span>}
            <a
                className="btn btn-link"
                href={opened.url}
                target="_blank"
                rel="noopener noreferrer"
                style={{ padding: '8px 14px' }}
            >
                Open at this spot ↗
            </a>
        </span>
    );
}

/**
 * Opens the excerpt's source at its exact location, or says why it cannot.
 *
 * The new tab is opened before the link is fetched, because a browser only allows a pop-up that
 * starts in the click itself; one opened after an await is blocked as an advert. A .pptx slide
 * closes it again and offers the deck instead.
 */
export function OpenSourceButton({ chunkId, label }: { chunkId: string; label?: string }) {
    const [note, setNote] = useState<string | null>(null);
    const [deck, setDeck] = useState<OpenedExcerpt | null>(null);

    return (
        <span>
            <button
                type="button"
                style={{ marginTop: 8 }}
                onClick={async () => {
                    setNote(null);
                    setDeck(null);
                    const tab = window.open('', '_blank');

                    try {
                        const opened = await api.get<OpenedExcerpt>(
                            `/sources/excerpts/${chunkId}/open`
                        );

                        if (isDeck(opened)) {
                            tab?.close();
                            setDeck(opened);
                        } else if (opened.url && tab) {
                            tab.location.href = opened.url;
                        } else {
                            tab?.close();
                            setNote(`${opened.sourceTitle}, ${opened.location}: ${opened.excerpt}`);
                        }
                    } catch (cause) {
                        tab?.close();
                        setNote(
                            cause instanceof Error ? cause.message : 'Could not open the source.'
                        );
                    }
                }}
            >
                {label ?? 'Open at this spot ↗'}
            </button>
            {deck && <DeckLink opened={deck} />}
            {note && (
                <p className="faint" style={{ margin: '6px 0 0', whiteSpace: 'pre-wrap' }}>
                    {note}
                </p>
            )}
        </span>
    );
}
