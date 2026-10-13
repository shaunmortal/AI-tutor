import { randomUUID } from 'node:crypto';
import type { CurriculumRepository } from '@/repositories/curriculumRepository.js';
import type {
    ChunkRow,
    NewChunk,
    SourceKind,
    SourceRepository,
    SourceRow,
} from '@/repositories/sourceRepository.js';
import type { AIAttachment, IAIProvider } from '@/services/ai/IAIProvider.js';
import {
    COURSE_MAP_SCHEMA,
    COURSE_MAP_SYSTEM,
    FIGURE_SYSTEM,
    PDF_FIGURES_SCHEMA,
    SLIDE_FIGURES_SCHEMA,
    courseMapPrompt,
} from '@/services/ai/prompts.js';
import { toSlug } from '@/services/learning/curriculumValidator.js';
import {
    extractPdfPages,
    extractPptxSlides,
    fetchYoutubeSegments,
    parseCaptions,
    splitText,
    windowTranscript,
    youtubeAt,
} from '@/services/sources/extractors.js';
import { AppError } from '@/utils/http.js';

/** Claude reads PDFs up to 100 pages in one request; past that, figures are skipped, text is not. */
const MAX_PDF_PAGES_FOR_FIGURES = 100;

/** Slide pictures described per request. */
const IMAGES_PER_CALL = 16;

/** How much of each excerpt the course-map call sees. Enough to recognise the topic. */
const MAP_PREVIEW_CHARS = 220;

/** Upper bound on the course-map prompt, so a 600-page book still fits in one request. */
const MAP_MAX_EXCERPTS = 450;

export interface Citation {
    n: number;
    chunkId: string;
    sourceId: string;
    sourceTitle: string;
    kind: SourceKind;
    modality: ChunkRow['modality'];
    /** "p. 42", "slide 7", "12:05" — what the chip shows. */
    location: string;
    excerpt: string;
}

/**
 * Course material in, located excerpts and a course map out.
 *
 * Ingestion runs after the upload request has returned, because a textbook with figures takes a
 * minute or two of model calls and nobody should hold a request open for that. The source row
 * carries the status, and the client polls the list.
 *
 * ponytail: in-process background work. One instance, so nothing is lost but a restart mid-ingest;
 * that source is left 'processing' and the learner re-uploads. Move to a queue if this scales out.
 */
export class SourceService {
    private readonly ai: IAIProvider;
    private readonly sources: SourceRepository;
    private readonly curricula: CurriculumRepository;

    constructor(ai: IAIProvider, sources: SourceRepository, curricula: CurriculumRepository) {
        this.ai = ai;
        this.sources = sources;
        this.curricula = curricula;
    }

    async uploadFile(input: {
        userId: string;
        kind: 'pdf' | 'slides';
        title: string;
        file: Uint8Array;
        fileName: string;
    }): Promise<SourceRow> {
        const format = fileFormat(input.file);

        if (!format || (input.kind === 'pdf' && format !== 'pdf')) {
            throw new AppError(
                400,
                'UNSUPPORTED_FILE',
                input.kind === 'pdf'
                    ? 'That file is not a PDF.'
                    : 'Slides must be a .pptx or a PDF export of the deck.'
            );
        }

        // The user id leads the path, so one learner's files can never be addressed by another's.
        const storagePath = `${input.userId}/${randomUUID()}.${format}`;

        await this.sources.uploadFile(
            storagePath,
            input.file,
            format === 'pdf'
                ? 'application/pdf'
                : 'application/vnd.openxmlformats-officedocument.presentationml.presentation'
        );

        const source = await this.sources.create({
            userId: input.userId,
            kind: input.kind,
            title: input.title,
            storagePath,
        });

        this.runInBackground(source, () => this.ingestFile(source, input.file, format));

        return source;
    }

    async addVideo(input: {
        userId: string;
        url: string;
        title: string;
        captions?: string;
    }): Promise<SourceRow> {
        const source = await this.sources.create({
            userId: input.userId,
            kind: 'video',
            title: input.title,
            videoUrl: input.url,
        });

        this.runInBackground(source, () => this.ingestVideo(source, input.captions));

        return source;
    }

    async list(userId: string) {
        return (await this.sources.list(userId)).map((source) => ({
            id: source.id,
            kind: source.kind,
            title: source.title,
            status: source.status,
            error: source.error,
            excerptCount: source.chunkCount,
            curriculumId: source.curriculum_id,
            createdAt: source.created_at,
        }));
    }

    async remove(sourceId: string, userId: string) {
        await this.sources.remove(sourceId, userId);
    }

    /**
     * Groups every ready excerpt into subjects and topics, with concepts and prerequisites, and
     * writes it as a curriculum the rest of the app already knows how to plan, test and track.
     *
     * Each call writes a new curriculum rather than editing the last one: topic ids carry mastery,
     * and silently re-pointing them would corrupt a learner's history.
     */
    async organize(userId: string, courseName: string) {
        const chunks = await this.sources.listChunks(userId);

        if (chunks.length === 0) {
            throw new AppError(
                409,
                'NO_MATERIAL',
                'Add at least one source and wait for it to finish processing first.'
            );
        }

        const sourceById = new Map(
            (await this.sources.findMany([...new Set(chunks.map((c) => c.source_id))])).map((s) => [
                s.id,
                s,
            ])
        );

        // Evenly spaced when there is more than fits, so every part of every source is represented.
        const step = Math.max(1, chunks.length / MAP_MAX_EXCERPTS);
        const shown: { index: number; chunk: ChunkRow }[] = [];
        for (let position = 0; position < chunks.length; position += step) {
            const index = Math.floor(position);
            shown.push({ index, chunk: chunks[index]! });
        }

        const response = await this.ai.generateJson({
            system: COURSE_MAP_SYSTEM,
            prompt: courseMapPrompt(
                courseName,
                shown.map(({ index, chunk }) => ({
                    id: index,
                    label: `${sourceById.get(chunk.source_id)?.title ?? 'source'}, ${locationOf(chunk)}`,
                    text: chunk.content.replace(/\s+/g, ' ').slice(0, MAP_PREVIEW_CHARS),
                }))
            ),
            schema: COURSE_MAP_SCHEMA,
            maxTokens: 16000,
        });

        const map = parseCourseMap(response.data);

        const curriculum = await this.curricula.create({
            slug: `${toSlug(courseName).slice(0, 60) || 'course'}-${randomUUID().slice(0, 8)}`,
            name: courseName.slice(0, 120),
            description: `Built from ${sourceById.size} uploaded source${sourceById.size === 1 ? '' : 's'}.`,
            subjects: map.subjects.map((subject) => ({
                name: subject.name,
                weight: subject.weight,
                topics: subject.topics.map((topic) => topic.name),
            })),
        });

        const courseMap = await this.sources.courseMap(curriculum.id, userId);
        const topicIdByName = new Map(
            courseMap.topics.filter((t) => t.parent_id).map((t) => [t.name, t.id])
        );

        // Every excerpt the model was shown gets the topic it chose.
        const topicOfIndex = new Map<number, string>();
        const prerequisitePairs: { topicId: string; prerequisiteId: string }[] = [];

        for (const subject of map.subjects) {
            for (const topic of subject.topics) {
                const topicId = topicIdByName.get(topic.name);
                if (!topicId) continue;

                await this.sources.setTopicDetails(topicId, topic.keyConcepts.slice(0, 8));

                for (const id of topic.excerptIds) topicOfIndex.set(id, topicId);

                for (const name of topic.prerequisites) {
                    const prerequisiteId = topicIdByName.get(name);
                    if (prerequisiteId && prerequisiteId !== topicId) {
                        prerequisitePairs.push({ topicId, prerequisiteId });
                    }
                }
            }
        }

        await this.sources.addPrerequisites(prerequisitePairs);
        await this.sources.tagChunks(assignTopics(chunks, topicOfIndex));
        await this.sources.attachCurriculum(userId, curriculum.id);

        return this.courseMap(curriculum.id, userId);
    }

    async courseMap(curriculumId: string, userId: string) {
        const map = await this.sources.courseMap(curriculumId, userId);
        const curriculum = await this.curricula.findById(curriculumId);

        const nameOf = new Map(map.topics.map((topic) => [topic.id, topic.name]));

        return {
            curriculum: curriculum && { id: curriculum.id, name: curriculum.name },
            subjects: map.topics
                .filter((topic) => !topic.parent_id)
                .map((subject) => ({
                    id: subject.id,
                    name: subject.name,
                    topics: map.topics
                        .filter((topic) => topic.parent_id === subject.id)
                        .map((topic) => ({
                            id: topic.id,
                            name: topic.name,
                            keyConcepts: topic.key_concepts,
                            excerptCount: map.excerptCounts.get(topic.id) ?? 0,
                            prerequisites: map.prerequisites
                                .filter((pair) => pair.topic_id === topic.id)
                                .map((pair) => ({
                                    id: pair.prerequisite_id,
                                    name: nameOf.get(pair.prerequisite_id) ?? '',
                                })),
                        })),
                })),
        };
    }

    /** Where a citation chip goes: the file at that page, or the video at that second. */
    async openExcerpt(chunkId: string, userId: string) {
        const chunk = await this.sources.findChunk(chunkId, userId);
        const source = await this.sources.find(chunk.source_id, userId);

        let url: string | null = null;

        if (source.video_url && chunk.start_sec !== null) {
            url = youtubeAt(source.video_url, chunk.start_sec);
        } else if (source.storage_path?.endsWith('.pdf')) {
            const page = chunk.page ?? chunk.slide;
            url = `${await this.sources.signedUrl(source.storage_path)}${page ? `#page=${page}` : ''}`;
        }

        return {
            url,
            sourceTitle: source.title,
            location: locationOf(chunk),
            excerpt: chunk.content,
        };
    }

    /** Excerpts for a question, numbered for the prompt and shaped for citation chips. */
    async retrieve(userId: string, query: string, limit = 6): Promise<Citation[]> {
        const rows = await this.sources.search(userId, query, limit);
        const sourceById = new Map(
            (await this.sources.findMany([...new Set(rows.map((row) => row.source_id))])).map(
                (source) => [source.id, source]
            )
        );

        return rows.map((row, index) => toCitation(index + 1, row, sourceById.get(row.source_id)));
    }

    async excerptsForTopic(topicId: string, userId: string, limit = 8): Promise<Citation[]> {
        const rows = await this.sources.chunksForTopic(topicId, userId, limit);
        const sourceById = new Map(
            (await this.sources.findMany([...new Set(rows.map((row) => row.source_id))])).map(
                (source) => [source.id, source]
            )
        );

        return rows.map((row, index) => toCitation(index + 1, row, sourceById.get(row.source_id)));
    }

    async hasMaterial(userId: string) {
        return this.sources.hasReadySources(userId);
    }

    private runInBackground(source: SourceRow, work: () => Promise<NewChunk[]>) {
        void (async () => {
            try {
                const chunks = await work();

                if (chunks.length === 0) {
                    await this.sources.setStatus(
                        source.id,
                        'failed',
                        'No readable content was found. Scanned PDFs need OCR first.'
                    );
                    return;
                }

                await this.sources.insertChunks(source, chunks);
                await this.sources.setStatus(source.id, 'ready');
            } catch (error) {
                const message = error instanceof Error ? error.message : String(error);
                console.error(`ingest ${source.id} failed: ${message}`);

                await this.sources.deleteChunks(source.id).catch(() => undefined);
                await this.sources
                    .setStatus(source.id, 'failed', message.slice(0, 300))
                    .catch(() => undefined);
            }
        })();
    }

    private async ingestFile(source: SourceRow, file: Uint8Array, format: 'pdf' | 'pptx') {
        if (format === 'pptx') return this.ingestPptx(file);

        // A deck exported to PDF is still a deck: one page is one slide.
        const asSlides = source.kind === 'slides';
        const pages = await extractPdfPages(file);

        const chunks: NewChunk[] = pages.flatMap((page) =>
            splitText(page.text).map((content) => ({
                modality: 'text' as const,
                ...(asSlides ? { slide: page.page } : { page: page.page }),
                content,
            }))
        );

        const pageCount = pages.at(-1)?.page ?? 0;

        if (pageCount > 0 && pageCount <= MAX_PDF_PAGES_FOR_FIGURES) {
            const figures = await this.describeFigures(
                [
                    {
                        kind: 'pdf',
                        mediaType: 'application/pdf',
                        base64: Buffer.from(file).toString('base64'),
                    },
                ],
                `Describe the figures in this ${asSlides ? 'slide deck (one page per slide)' : 'document'}. Give the page number each one is on.`,
                PDF_FIGURES_SCHEMA
            );

            for (const figure of figures) {
                const page = figure.page;
                if (!Number.isInteger(page) || page! < 1 || page! > pageCount) continue;

                chunks.push({
                    modality: 'figure',
                    ...(asSlides ? { slide: page } : { page }),
                    content: `Figure: ${figure.description}`,
                });
            }
        }

        return chunks;
    }

    private async ingestPptx(file: Uint8Array): Promise<NewChunk[]> {
        const slides = await extractPptxSlides(file);

        const chunks: NewChunk[] = slides.flatMap((slide) =>
            slide.text
                ? splitText(slide.text).map((content) => ({
                      modality: 'text' as const,
                      slide: slide.slide,
                      content,
                  }))
                : []
        );

        const images: AIAttachment[] = slides.flatMap((slide) =>
            slide.images.map((image, index) => ({
                kind: 'image' as const,
                mediaType: image.mediaType,
                base64: image.base64,
                label: `Slide ${slide.slide}, picture ${index + 1}:`,
            }))
        );

        for (let start = 0; start < images.length; start += IMAGES_PER_CALL) {
            const figures = await this.describeFigures(
                images.slice(start, start + IMAGES_PER_CALL),
                'Describe the pictures above. Each is labelled with the slide it is on; give that slide number.',
                SLIDE_FIGURES_SCHEMA
            );

            for (const figure of figures) {
                if (!Number.isInteger(figure.slide) || figure.slide! < 1) continue;

                chunks.push({
                    modality: 'figure',
                    slide: figure.slide,
                    content: `Figure: ${figure.description}`,
                });
            }
        }

        return chunks;
    }

    private async ingestVideo(source: SourceRow, captions?: string): Promise<NewChunk[]> {
        const segments = captions
            ? parseCaptions(captions)
            : await fetchYoutubeSegments(source.video_url!).catch((error: unknown) => {
                  throw new Error(
                      `Could not fetch captions for this video (${
                          error instanceof Error ? error.message : String(error)
                      }). Upload its .vtt or .srt captions instead.`
                  );
              });

        return windowTranscript(segments).map((window) => ({
            modality: 'transcript' as const,
            startSec: window.startSec,
            endSec: window.endSec,
            content: window.text,
        }));
    }

    /**
     * Figures are worth searching but not worth failing over: a model outage here leaves the
     * source with its text, which is most of what a learner needs.
     */
    private async describeFigures(
        attachments: AIAttachment[],
        prompt: string,
        schema: Record<string, unknown>
    ): Promise<{ description: string; page?: number; slide?: number }[]> {
        try {
            const response = await this.ai.generateJson({
                system: FIGURE_SYSTEM,
                prompt,
                schema,
                attachments,
                quality: 'standard',
                maxTokens: 8000,
            });

            const figures = (response.data as { figures?: unknown }).figures;

            return Array.isArray(figures)
                ? figures.filter(
                      (figure): figure is { description: string } =>
                          typeof figure?.description === 'string' && figure.description.trim() !== ''
                  )
                : [];
        } catch (error) {
            console.warn(
                `figure description skipped: ${error instanceof Error ? error.message : String(error)}`
            );
            return [];
        }
    }
}

export function locationOf(chunk: Pick<ChunkRow, 'page' | 'slide' | 'start_sec'>): string {
    if (chunk.start_sec !== null && chunk.start_sec !== undefined) return clock(chunk.start_sec);
    if (chunk.slide !== null && chunk.slide !== undefined) return `slide ${chunk.slide}`;
    return `p. ${chunk.page}`;
}

function toCitation(n: number, row: ChunkRow, source: SourceRow | undefined): Citation {
    return {
        n,
        chunkId: row.id,
        sourceId: row.source_id,
        sourceTitle: source?.title ?? 'Course material',
        kind: source?.kind ?? 'pdf',
        modality: row.modality,
        location: locationOf(row),
        excerpt: row.content,
    };
}

function clock(totalSeconds: number): string {
    const hours = Math.floor(totalSeconds / 3600);
    const minutes = Math.floor((totalSeconds % 3600) / 60);
    const seconds = totalSeconds % 60;
    const mm = String(minutes).padStart(hours ? 2 : 1, '0');

    return `${hours ? `${hours}:` : ''}${mm}:${String(seconds).padStart(2, '0')}`;
}

/** Magic bytes, not the file name: a renamed file is still what it is. */
function fileFormat(file: Uint8Array): 'pdf' | 'pptx' | null {
    const head = String.fromCharCode(...file.slice(0, 4));

    if (head === '%PDF') return 'pdf';
    // A .pptx is a zip; whether it is really a deck is found out when it is opened.
    if (head === 'PK\u0003\u0004') return 'pptx';
    return null;
}

interface CourseMap {
    subjects: {
        name: string;
        weight: number;
        topics: { name: string; keyConcepts: string[]; prerequisites: string[]; excerptIds: number[] }[];
    }[];
}

/** The reply becomes topics other features act on, so it is checked rather than trusted. */
export function parseCourseMap(raw: unknown): CourseMap {
    const subjects = (raw as CourseMap | null)?.subjects;

    if (!Array.isArray(subjects) || subjects.length === 0) {
        throw new AppError(502, 'AI_MALFORMED', 'The course map came back empty. Try again.');
    }

    const seenTopics = new Set<string>();
    const seenSubjects = new Set<string>();

    const cleaned = subjects
        .map((subject) => ({
            name: String(subject.name ?? '').trim().slice(0, 120),
            weight: Math.min(5, Math.max(1, Math.round(Number(subject.weight) || 1))),
            topics: (Array.isArray(subject.topics) ? subject.topics : [])
                .map((topic) => ({
                    name: String(topic.name ?? '').trim().slice(0, 120),
                    keyConcepts: (topic.keyConcepts ?? []).map(String).filter(Boolean),
                    prerequisites: (topic.prerequisites ?? []).map(String),
                    excerptIds: (topic.excerptIds ?? []).filter(Number.isInteger),
                }))
                // Topic names must be unique across the course: they are how prerequisites refer.
                .filter((topic) => topic.name && !seenTopics.has(topic.name) && seenTopics.add(topic.name)),
        }))
        .filter(
            (subject) =>
                subject.name &&
                subject.topics.length > 0 &&
                !seenSubjects.has(subject.name) &&
                seenSubjects.add(subject.name)
        );

    if (cleaned.length === 0) {
        throw new AppError(502, 'AI_MALFORMED', 'The course map had no usable topics. Try again.');
    }

    return { subjects: cleaned };
}

/**
 * Every excerpt gets a topic: the one the model chose, or, for excerpts it was not shown, the topic
 * of the nearest shown excerpt in the same source. Neighbouring pages of a book are almost always
 * on the same topic, so this is right far more often than it is wrong, and it costs nothing.
 */
export function assignTopics(
    chunks: Pick<ChunkRow, 'id' | 'source_id'>[],
    topicOfIndex: Map<number, string>
): { chunkId: string; topicId: string }[] {
    return chunks.flatMap((chunk, index) => {
        if (topicOfIndex.has(index)) return [{ chunkId: chunk.id, topicId: topicOfIndex.get(index)! }];

        for (let distance = 1; distance < chunks.length; distance += 1) {
            for (const neighbour of [index - distance, index + distance]) {
                const topicId = topicOfIndex.get(neighbour);

                if (topicId && chunks[neighbour]?.source_id === chunk.source_id) {
                    return [{ chunkId: chunk.id, topicId }];
                }
            }
        }

        return [];
    });
}
