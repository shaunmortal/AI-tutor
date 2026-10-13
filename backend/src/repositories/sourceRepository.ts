import type { SupabaseClient } from '@supabase/supabase-js';
import { AppError } from '@/utils/http.js';

export type SourceKind = 'pdf' | 'slides' | 'video';
export type ChunkModality = 'text' | 'figure' | 'transcript';

export interface SourceRow {
    id: string;
    user_id: string;
    curriculum_id: string | null;
    kind: SourceKind;
    title: string;
    storage_path: string | null;
    video_url: string | null;
    status: 'processing' | 'ready' | 'failed';
    error: string | null;
    created_at: string;
}

export interface ChunkRow {
    id: string;
    source_id: string;
    topic_id: string | null;
    modality: ChunkModality;
    page: number | null;
    slide: number | null;
    start_sec: number | null;
    end_sec: number | null;
    content: string;
    rank?: number;
}

export interface NewChunk {
    modality: ChunkModality;
    page?: number | null;
    slide?: number | null;
    startSec?: number | null;
    endSec?: number | null;
    content: string;
}

const BUCKET = 'course-sources';

const SOURCE_COLUMNS =
    'id, user_id, curriculum_id, kind, title, storage_path, video_url, status, error, created_at';
const CHUNK_COLUMNS = 'id, source_id, topic_id, modality, page, slide, start_sec, end_sec, content';

/**
 * Course material and its excerpts.
 *
 * Always used with the elevated client: learners may read their own rows, but writing them only
 * happens after a file has been parsed on the server. Every query is filtered by user id from the
 * token, never from the request body.
 */
export class SourceRepository {
    private readonly db: SupabaseClient;

    constructor(db: SupabaseClient) {
        this.db = db;
    }

    async create(input: {
        userId: string;
        kind: SourceKind;
        title: string;
        storagePath?: string | null;
        videoUrl?: string | null;
    }): Promise<SourceRow> {
        const { data, error } = await this.db
            .from('course_sources')
            .insert({
                user_id: input.userId,
                kind: input.kind,
                title: input.title,
                storage_path: input.storagePath ?? null,
                video_url: input.videoUrl ?? null,
            })
            .select(SOURCE_COLUMNS)
            .single();

        if (error) throw error;
        return data as SourceRow;
    }

    async list(userId: string): Promise<(SourceRow & { chunkCount: number })[]> {
        const { data, error } = await this.db
            .from('course_sources')
            .select(`${SOURCE_COLUMNS}, source_chunks(count)`)
            .eq('user_id', userId)
            .order('created_at', { ascending: false });

        if (error) throw error;

        return ((data ?? []) as (SourceRow & { source_chunks: { count: number }[] })[]).map(
            ({ source_chunks, ...row }) => ({ ...row, chunkCount: source_chunks[0]?.count ?? 0 })
        );
    }

    async find(sourceId: string, userId: string): Promise<SourceRow> {
        const { data, error } = await this.db
            .from('course_sources')
            .select(SOURCE_COLUMNS)
            .eq('id', sourceId)
            .eq('user_id', userId)
            .maybeSingle();

        if (error) throw error;
        if (!data) throw new AppError(404, 'SOURCE_NOT_FOUND', 'That source does not exist.');

        return data as SourceRow;
    }

    async findMany(sourceIds: string[]): Promise<SourceRow[]> {
        if (sourceIds.length === 0) return [];

        const { data, error } = await this.db
            .from('course_sources')
            .select(SOURCE_COLUMNS)
            .in('id', sourceIds);

        if (error) throw error;
        return (data ?? []) as SourceRow[];
    }

    async setStatus(sourceId: string, status: SourceRow['status'], message: string | null = null) {
        const { error } = await this.db
            .from('course_sources')
            .update({ status, error: message })
            .eq('id', sourceId);

        if (error) throw error;
    }

    async remove(sourceId: string, userId: string) {
        const source = await this.find(sourceId, userId);

        if (source.storage_path) {
            await this.db.storage.from(BUCKET).remove([source.storage_path]);
        }

        const { error } = await this.db.from('course_sources').delete().eq('id', sourceId);
        if (error) throw error;
    }

    async uploadFile(path: string, file: Uint8Array, contentType: string) {
        const { error } = await this.db.storage
            .from(BUCKET)
            .upload(path, file, { contentType, upsert: false });

        if (error) throw error;
    }

    async downloadFile(path: string): Promise<Uint8Array> {
        const { data, error } = await this.db.storage.from(BUCKET).download(path);

        if (error) throw error;
        return new Uint8Array(await data.arrayBuffer());
    }

    /** A link to the file that works for an hour: long enough to read, short enough not to leak. */
    async signedUrl(path: string): Promise<string> {
        const { data, error } = await this.db.storage.from(BUCKET).createSignedUrl(path, 60 * 60);

        if (error) throw error;
        return data.signedUrl;
    }

    async insertChunks(source: SourceRow, chunks: NewChunk[]) {
        // Batches keep each request under PostgREST's body limit on a 400-page book.
        for (let start = 0; start < chunks.length; start += 200) {
            const { error } = await this.db.from('source_chunks').insert(
                chunks.slice(start, start + 200).map((chunk) => ({
                    source_id: source.id,
                    user_id: source.user_id,
                    modality: chunk.modality,
                    page: chunk.page ?? null,
                    slide: chunk.slide ?? null,
                    start_sec: chunk.startSec ?? null,
                    end_sec: chunk.endSec ?? null,
                    content: chunk.content,
                }))
            );

            if (error) throw error;
        }
    }

    async deleteChunks(sourceId: string) {
        const { error } = await this.db.from('source_chunks').delete().eq('source_id', sourceId);
        if (error) throw error;
    }

    /** Every excerpt from the learner's ready sources, in reading order. */
    async listChunks(userId: string): Promise<ChunkRow[]> {
        const rows: ChunkRow[] = [];

        // PostgREST caps a response at 1000 rows by default, so page through.
        for (let from = 0; ; from += 1000) {
            const { data, error } = await this.db
                .from('source_chunks')
                .select(CHUNK_COLUMNS)
                .eq('user_id', userId)
                .order('source_id')
                .order('page', { nullsFirst: true })
                .order('slide', { nullsFirst: true })
                .order('start_sec', { nullsFirst: true })
                .range(from, from + 999);

            if (error) throw error;

            rows.push(...((data ?? []) as ChunkRow[]));
            if (!data || data.length < 1000) return rows;
        }
    }

    async findChunk(chunkId: string, userId: string): Promise<ChunkRow> {
        const { data, error } = await this.db
            .from('source_chunks')
            .select(CHUNK_COLUMNS)
            .eq('id', chunkId)
            .eq('user_id', userId)
            .maybeSingle();

        if (error) throw error;
        if (!data) throw new AppError(404, 'EXCERPT_NOT_FOUND', 'That excerpt does not exist.');

        return data as ChunkRow;
    }

    async chunksForTopic(topicId: string, userId: string, limit = 8): Promise<ChunkRow[]> {
        const { data, error } = await this.db
            .from('source_chunks')
            .select(CHUNK_COLUMNS)
            .eq('topic_id', topicId)
            .eq('user_id', userId)
            .limit(limit);

        if (error) throw error;
        return (data ?? []) as ChunkRow[];
    }

    async search(userId: string, query: string, limit = 6): Promise<ChunkRow[]> {
        const { data, error } = await this.db.rpc('search_source_chunks', {
            p_user_id: userId,
            p_query: query,
            p_limit: limit,
        });

        if (error) throw error;
        return (data ?? []) as ChunkRow[];
    }

    async tagChunks(assignments: { chunkId: string; topicId: string }[]) {
        const byTopic = new Map<string, string[]>();

        for (const { chunkId, topicId } of assignments) {
            byTopic.set(topicId, [...(byTopic.get(topicId) ?? []), chunkId]);
        }

        for (const [topicId, chunkIds] of byTopic) {
            for (let start = 0; start < chunkIds.length; start += 200) {
                const { error } = await this.db
                    .from('source_chunks')
                    .update({ topic_id: topicId })
                    .in('id', chunkIds.slice(start, start + 200));

                if (error) throw error;
            }
        }
    }

    async attachCurriculum(userId: string, curriculumId: string) {
        const { error } = await this.db
            .from('course_sources')
            .update({ curriculum_id: curriculumId })
            .eq('user_id', userId)
            .eq('status', 'ready');

        if (error) throw error;
    }

    async hasReadySources(userId: string): Promise<boolean> {
        const { count, error } = await this.db
            .from('course_sources')
            .select('id', { count: 'exact', head: true })
            .eq('user_id', userId)
            .eq('status', 'ready');

        if (error) throw error;
        return (count ?? 0) > 0;
    }

    async setTopicDetails(
        topicId: string,
        keyConcepts: string[]
    ) {
        const { error } = await this.db
            .from('topics')
            .update({ key_concepts: keyConcepts })
            .eq('id', topicId);

        if (error) throw error;
    }

    async addPrerequisites(pairs: { topicId: string; prerequisiteId: string }[]) {
        if (pairs.length === 0) return;

        const { error } = await this.db.from('topic_prerequisites').upsert(
            pairs.map((pair) => ({ topic_id: pair.topicId, prerequisite_id: pair.prerequisiteId })),
            { ignoreDuplicates: true }
        );

        if (error) throw error;
    }

    /** The course map: topics, their concepts, prerequisites and how much material backs each. */
    async courseMap(curriculumId: string, userId: string) {
        const { data: topics, error } = await this.db
            .from('topics')
            .select('id, name, parent_id, key_concepts, sort_order')
            .eq('curriculum_id', curriculumId)
            .order('sort_order');

        if (error) throw error;

        const ids = ((topics ?? []) as { id: string }[]).map((topic) => topic.id);

        const [{ data: prereqs, error: prereqError }, { data: chunks, error: chunkError }] =
            await Promise.all([
                this.db.from('topic_prerequisites').select('topic_id, prerequisite_id').in('topic_id', ids),
                this.db
                    .from('source_chunks')
                    .select('topic_id')
                    .eq('user_id', userId)
                    .in('topic_id', ids),
            ]);

        if (prereqError) throw prereqError;
        if (chunkError) throw chunkError;

        const counts = new Map<string, number>();
        for (const row of (chunks ?? []) as { topic_id: string }[]) {
            counts.set(row.topic_id, (counts.get(row.topic_id) ?? 0) + 1);
        }

        return {
            topics: (topics ?? []) as {
                id: string;
                name: string;
                parent_id: string | null;
                key_concepts: string[];
                sort_order: number;
            }[],
            prerequisites: (prereqs ?? []) as { topic_id: string; prerequisite_id: string }[],
            excerptCounts: counts,
        };
    }
}
