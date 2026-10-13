-- 029_chunk_embeddings.sql
--
-- Hybrid retrieval: full-text search misses paraphrased questions ("how do I find something in a
-- sorted list" vs "binary search"). Each excerpt gets an embedding (Gemini, 768 dimensions,
-- written by the API at ingestion or by cmd/backfill-embeddings), and the tutor's search fuses
-- the full-text ranking with the vector ranking by Reciprocal Rank Fusion.
--
-- The embedding is nullable: an excerpt whose embedding failed (or predates this migration) is
-- still found by its words. search_source_chunks stays as the fallback when there is no
-- embedding key or the question could not be embedded.

create extension if not exists vector with schema extensions;

alter table public.source_chunks
    add column if not exists embedding extensions.vector(768);

-- Not granted to authenticated: learners read excerpts through a column grant (023) that leaves
-- this out, and a vector is no use to a browser.

create index if not exists source_chunks_embedding_idx
    on public.source_chunks using hnsw (embedding extensions.vector_cosine_ops);

/**
 * Ranked excerpts for one learner, by full-text rank and vector similarity together.
 *
 * Each side contributes its top 40 for this learner; an excerpt scores sum(1 / (60 + rank)) over
 * the lists it appears in (RRF, k = 60 as in Cormack et al.), so agreement between the two beats
 * a high place in one. Rank scales differ (ts_rank_cd vs cosine), which is why ranks, not
 * scores, are fused. The query words are OR-ed as in search_source_chunks.
 *
 * iterative_scan: the HNSW index is shared by every learner; without it a filtered scan can stop
 * at ef_search candidates that belong to other learners and return too few.
 */
create or replace function public.hybrid_search_source_chunks(
    p_user_id uuid,
    p_query text,
    p_embedding extensions.vector(768),
    p_limit integer default 6
)
returns table (
    id uuid,
    source_id uuid,
    topic_id uuid,
    modality text,
    page integer,
    slide integer,
    start_sec integer,
    end_sec integer,
    content text,
    rank real
)
language sql
stable
set search_path = public, extensions, pg_temp
set hnsw.iterative_scan = relaxed_order
as $$
    with q as (
        select nullif(replace(plainto_tsquery('english', p_query)::text, '&', '|'), '')::tsquery
            as query
    ),
    lexical as (
        select l.id, row_number() over (order by l.score desc) as r
        from (
            select c.id, ts_rank_cd(c.tsv, q.query) as score
            from public.source_chunks c, q
            where c.user_id = p_user_id
              and q.query is not null
              and c.tsv @@ q.query
            order by score desc
            limit 40
        ) l
    ),
    semantic as (
        select s.id, row_number() over (order by s.distance) as r
        from (
            select c.id, c.embedding <=> p_embedding as distance
            from public.source_chunks c
            where c.user_id = p_user_id
              and c.embedding is not null
            order by distance
            limit 40
        ) s
    ),
    fused as (
        select u.id, sum(1.0 / (60 + u.r)) as score
        from (select * from lexical union all select * from semantic) u
        group by u.id
    )
    select c.id, c.source_id, c.topic_id, c.modality, c.page, c.slide, c.start_sec, c.end_sec,
           c.content, f.score::real as rank
    from fused f
    join public.source_chunks c on c.id = f.id
    order by f.score desc, c.id
    limit greatest(1, least(p_limit, 20));
$$;

revoke all on function public.hybrid_search_source_chunks(uuid, text, extensions.vector, integer)
    from public, anon, authenticated;
