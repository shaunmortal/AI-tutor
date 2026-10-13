import { useState } from 'react';
import { api } from '@/lib/api';
import type { Citation, OpenedExcerpt } from '@/types/api';

/**
 * Where an answer came from: one chip per cited excerpt.
 *
 * Clicking a chip opens the source at that spot — the PDF at its page, the video at its second.
 * The new tab is opened before the link is fetched, because a browser only allows a pop-up that
 * starts in the click itself; one opened after an await is blocked as an advert.
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
                        <OpenSourceButton chunkId={citation.chunkId} />
                    </div>
                ))}
        </div>
    );
}

/** Opens the excerpt's source at its exact location, or says why it cannot. */
export function OpenSourceButton({ chunkId, label }: { chunkId: string; label?: string }) {
    const [note, setNote] = useState<string | null>(null);

    return (
        <span>
            <button
                type="button"
                style={{ marginTop: 8 }}
                onClick={async () => {
                    setNote(null);
                    const tab = window.open('', '_blank');

                    try {
                        const opened = await api.get<OpenedExcerpt>(
                            `/sources/excerpts/${chunkId}/open`
                        );

                        if (opened.url && tab) {
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
            {note && (
                <p className="faint" style={{ margin: '6px 0 0', whiteSpace: 'pre-wrap' }}>
                    {note}
                </p>
            )}
        </span>
    );
}
