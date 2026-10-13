import { useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import type { CourseMap } from '@/types/api';
import {
    bandOf,
    HEAD,
    layoutCourseMap,
    masteryLabel,
    NODE_H,
    NODE_W,
    practiceHref,
    type PlacedTopic,
} from './layout';

/**
 * The course map as a graph: subjects as columns, topics as nodes, prerequisites as arrows, each
 * node tinted by the learner's mastery.
 *
 * Hover, focus or a first tap shows a topic's key concepts and what it needs first; a click (or a
 * second tap on a phone) starts practice on it. Wider than a phone, so it scrolls sideways inside
 * its own box rather than shrinking the text to nothing.
 */

export function CourseGraph({
    map,
    mastery,
}: {
    map: CourseMap;
    mastery: Map<string, number | null>;
}) {
    const navigate = useNavigate();
    const layout = useMemo(() => layoutCourseMap(map), [map]);
    const [active, setActive] = useState<string | null>(null);
    const lastPointer = useRef<string>('mouse');
    // Whether the tapped node was already showing its details when the tap began. Focus lands on
    // pointer-down, before the click, so the click alone cannot tell a first tap from a second.
    const wasActive = useRef(false);

    const shown = layout.nodes.find((n) => n.topic.id === active) ?? null;
    const unlocks = shown
        ? layout.nodes.filter((n) => n.topic.prerequisites.some((p) => p.id === shown.topic.id))
        : [];

    function open(node: PlacedTopic) {
        navigate(practiceHref(node.subjectId, node.topic.id));
    }

    return (
        <div
            className="cmap"
            onPointerLeave={(e) => {
                if (e.pointerType === 'mouse') setActive(null);
            }}
            onKeyDown={(e) => {
                if (e.key === 'Escape') setActive(null);
            }}
        >
            <div className="cmap__canvas" style={{ width: layout.width, height: layout.height }}>
                {layout.columns.map((c) => (
                    <span
                        key={c.id}
                        className="cmap__subject label"
                        style={{ left: c.x, width: c.width, height: HEAD }}
                    >
                        {c.name}
                    </span>
                ))}

                <svg
                    className="cmap__edges"
                    width={layout.width}
                    height={layout.height}
                    aria-hidden="true"
                >
                    <defs>
                        {['', '-lit'].map((v) => (
                            <marker
                                key={v}
                                id={`cmap-arrow${v}`}
                                className={`cmap__arrow${v}`}
                                viewBox="0 0 10 10"
                                refX="8"
                                refY="5"
                                markerWidth="7"
                                markerHeight="7"
                                orient="auto"
                            >
                                <path d="M0 0L10 5L0 10z" />
                            </marker>
                        ))}
                    </defs>
                    {layout.edges.map((edge) => {
                        const lit = active !== null && (edge.from === active || edge.to === active);
                        return (
                            <path
                                key={`${edge.from}-${edge.to}`}
                                d={edge.d}
                                className={`cmap__edge${lit ? ' cmap__edge--lit' : ''}${active !== null && !lit ? ' cmap__edge--dim' : ''}`}
                                markerEnd={`url(#cmap-arrow${lit ? '-lit' : ''})`}
                            />
                        );
                    })}
                </svg>

                {layout.nodes.map((node) => {
                    const score = mastery.get(node.topic.id);
                    const needs = node.topic.prerequisites.map((p) => p.name);
                    return (
                        <button
                            key={node.topic.id}
                            type="button"
                            className={`cmap__node cmap__node--${bandOf(score)}`}
                            aria-current={node.topic.id === active ? 'true' : undefined}
                            aria-label={`${node.topic.name}. ${masteryLabel(score)}.${needs.length ? ` Needs first: ${needs.join(', ')}.` : ''} Practise this topic.`}
                            style={{ left: node.x, top: node.y, width: NODE_W, height: NODE_H }}
                            onPointerDown={(e) => {
                                lastPointer.current = e.pointerType;
                                wasActive.current = active === node.topic.id;
                            }}
                            onPointerEnter={(e) => {
                                if (e.pointerType === 'mouse') setActive(node.topic.id);
                            }}
                            onFocus={() => setActive(node.topic.id)}
                            onClick={() => {
                                // A first tap on a touch screen shows the details; the second opens.
                                if (lastPointer.current !== 'mouse' && !wasActive.current) {
                                    setActive(node.topic.id);
                                    lastPointer.current = 'mouse';
                                    return;
                                }
                                open(node);
                            }}
                        >
                            <span className="cmap__name">{node.topic.name}</span>
                            <span className="cmap__score">{masteryLabel(score)}</span>
                        </button>
                    );
                })}

                {shown && (
                    <div
                        className="cmap__pop card"
                        style={{
                            left: Math.max(4, Math.min(shown.x - 50, layout.width - 244)),
                            // Below the topic in the top half, above it in the bottom half, so it
                            // never hangs off the end of the scroll box.
                            ...(shown.y < layout.height / 2
                                ? { top: shown.y + NODE_H + 8 }
                                : { bottom: layout.height - shown.y + 8 }),
                        }}
                    >
                        <strong>{shown.topic.name}</strong>
                        <span className="faint">
                            {shown.subjectName} · {masteryLabel(mastery.get(shown.topic.id))}
                        </span>
                        {shown.topic.keyConcepts.length > 0 && (
                            <div className="cmap__concepts">
                                {shown.topic.keyConcepts.map((k) => (
                                    <span key={k} className="pill">
                                        {k}
                                    </span>
                                ))}
                            </div>
                        )}
                        <span className="faint">
                            Needs first:{' '}
                            {shown.topic.prerequisites.length
                                ? shown.topic.prerequisites.map((p) => p.name).join(', ')
                                : 'nothing — a starting point'}
                        </span>
                        {unlocks.length > 0 && (
                            <span className="faint">
                                Unlocks: {unlocks.map((n) => n.topic.name).join(', ')}
                            </span>
                        )}
                        <button type="button" className="primary" onClick={() => open(shown)}>
                            Practise this topic →
                        </button>
                    </div>
                )}
            </div>
        </div>
    );
}

/** The colour key, worded so it does not rely on colour alone. */
export function MasteryLegend() {
    return (
        <div className="cmap__legend faint" aria-hidden="true">
            {(
                [
                    ['strong', 'Strong (70+)'],
                    ['mid', 'Getting there'],
                    ['weak', 'Weak (under 40)'],
                    ['none', 'Not started'],
                ] as const
            ).map(([band, label]) => (
                <span key={band} className="row" style={{ gap: 6 }}>
                    <span className={`cmap__swatch cmap__node--${band}`} />
                    {label}
                </span>
            ))}
        </div>
    );
}
