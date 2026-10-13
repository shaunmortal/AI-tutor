import type { CourseMap } from '@/types/api';

/**
 * Where each topic of a course map sits when drawn as a graph.
 *
 * Subjects are columns (two topics wide where a layer needs it); rows are prerequisite layers — a topic sits one layer below the deepest
 * topic it needs first — so every arrow points down and "what to learn before this" reads top to
 * bottom. Plain arithmetic rather than a graph-layout library: a course map is tens of topics, and
 * a library would outweigh the screen it draws on.
 */

export const NODE_W = 140;
export const NODE_H = 70;
/** Between subjects. */
const GAP_X = 28;
/** Between topics side by side within one subject. */
const GAP_SUB = 12;
const GAP_Y = 14;
/**
 * At most this many topics of one subject sit side by side in a layer; more wrap to another row.
 * Side by side keeps an arrow between consecutive layers from running behind a sibling topic.
 */
const MAX_SIDE = 2;
/** Extra space between prerequisite layers, so layers read as bands rather than one grid. */
const LAYER_GAP = 26;
const PAD = 12;
/** Room above the first row for the subject headings. */
export const HEAD = 34;

type Topic = CourseMap['subjects'][number]['topics'][number];

export interface PlacedTopic {
    topic: Topic;
    subjectId: string;
    subjectName: string;
    layer: number;
    x: number;
    y: number;
}

export interface Edge {
    from: string;
    to: string;
    /** SVG path, from the bottom of the prerequisite to the top of the topic that needs it. */
    d: string;
}

export interface GraphLayout {
    nodes: PlacedTopic[];
    edges: Edge[];
    columns: { id: string; name: string; x: number; width: number }[];
    width: number;
    height: number;
}

/**
 * Each topic's layer: 0 with no prerequisites in the map, else one below its deepest prerequisite.
 *
 * Prerequisites outside the map are ignored, and a cycle (which the model should never write, but
 * could) is broken where it is found rather than recursing forever.
 */
export function layers(topics: Topic[]): Map<string, number> {
    const byId = new Map(topics.map((t) => [t.id, t]));
    const layer = new Map<string, number>();
    const visiting = new Set<string>();

    function visit(id: string): number {
        const known = layer.get(id);
        if (known !== undefined) return known;
        if (visiting.has(id)) return -1; // cycle: this edge does not push the layer down

        visiting.add(id);
        let deepest = -1;
        for (const p of byId.get(id)?.prerequisites ?? []) {
            if (byId.has(p.id)) deepest = Math.max(deepest, visit(p.id));
        }
        visiting.delete(id);

        layer.set(id, deepest + 1);
        return deepest + 1;
    }

    topics.forEach((t) => visit(t.id));
    return layer;
}

export function layoutCourseMap(map: CourseMap): GraphLayout {
    const all = map.subjects.flatMap((s) => s.topics);
    const layerOf = layers(all);
    const depth = Math.max(0, ...layerOf.values()) + 1;

    const countIn = (subject: CourseMap['subjects'][number], l: number) =>
        subject.topics.filter((t) => layerOf.get(t.id) === l).length;
    // Each subject is as many topics wide as its busiest layer, up to MAX_SIDE.
    const wide = map.subjects.map((s) =>
        Math.min(MAX_SIDE, Math.max(1, ...Array.from({ length: depth }, (_, l) => countIn(s, l))))
    );
    // How many rows each layer needs: the most any one subject wraps to in it.
    const rows = Array.from({ length: depth }, (_, l) =>
        Math.max(1, ...map.subjects.map((s, i) => Math.ceil(countIn(s, l) / wide[i]!)))
    );
    const layerY: number[] = [];
    let y = PAD + HEAD;
    for (let l = 0; l < depth; l += 1) {
        layerY.push(y);
        y += rows[l]! * (NODE_H + GAP_Y) + LAYER_GAP;
    }

    const nodes: PlacedTopic[] = [];
    let x = PAD;
    const columns = map.subjects.map((subject, col) => {
        const left = x;
        const used = new Array<number>(depth).fill(0);

        for (const topic of subject.topics) {
            const layer = layerOf.get(topic.id) ?? 0;
            const k = used[layer]!;
            nodes.push({
                topic,
                subjectId: subject.id,
                subjectName: subject.name,
                layer,
                x: left + (k % wide[col]!) * (NODE_W + GAP_SUB),
                y: layerY[layer]! + Math.floor(k / wide[col]!) * (NODE_H + GAP_Y),
            });
            used[layer] = k + 1;
        }

        const width = wide[col]! * NODE_W + (wide[col]! - 1) * GAP_SUB;
        x += width + GAP_X;
        return { id: subject.id, name: subject.name, x: left, width };
    });

    const at = new Map(nodes.map((n) => [n.topic.id, n]));
    const edges: Edge[] = [];
    for (const node of nodes) {
        for (const p of node.topic.prerequisites) {
            const from = at.get(p.id);
            if (!from || from.layer >= node.layer) continue; // outside the map, or a broken cycle

            const x1 = from.x + NODE_W / 2;
            const y1 = from.y + NODE_H;
            const x2 = node.x + NODE_W / 2;
            const y2 = node.y - 4; // stop short, so the arrowhead is not under the node
            const bend = Math.max(24, (y2 - y1) / 2);
            edges.push({
                from: p.id,
                to: node.topic.id,
                d: `M${x1} ${y1} C${x1} ${y1 + bend} ${x2} ${y2 - bend} ${x2} ${y2}`,
            });
        }
    }

    return {
        nodes,
        edges,
        columns,
        width: Math.max(PAD * 2, x - GAP_X + PAD),
        height: y - LAYER_GAP + PAD,
    };
}

export type Band = 'strong' | 'mid' | 'weak' | 'none';

export function bandOf(mastery: number | null | undefined): Band {
    if (mastery === null || mastery === undefined) return 'none';
    return mastery >= 70 ? 'strong' : mastery >= 40 ? 'mid' : 'weak';
}

export function masteryLabel(mastery: number | null | undefined): string {
    return mastery === null || mastery === undefined
        ? 'Not started'
        : `Mastery ${Math.round(mastery)}`;
}

/** Practice for one topic. The subject scopes the session; the topic is passed along with it. */
export function practiceHref(subjectId: string, topicId: string): string {
    return `/practice?subject=${subjectId}&topic=${topicId}`;
}
