-- 030_hybrid_similarity_floor.sql
--
-- Hybrid search used to return its top excerpts for any question, however unrelated, because the
-- vector side always has a nearest neighbour. The vector side now only contributes excerpts whose
-- cosine similarity to the question is at least 0.67; full-text matches are kept regardless.
--
-- The floor was chosen from data (docs/grounding.md, "Similarity floor"): on the MIT 6.006 eval
-- set with gemini-embedding-2 (768 dimensions, question/document task prefixes), the best excerpt
-- of every off-material question scored 0.565-0.664, and the best expected excerpt of every
-- in-material question scored 0.677-0.842. The number belongs to that embedding model: re-measure
-- it (cmd/evalgrounding -dry-run) if the model changes.
--
-- Same signature as 029, so callers are unchanged. The filter sits outside the inner
-- "order by distance limit 40" so the HNSW index scan is the same as before.

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
        where s.distance <= 1 - 0.67   -- the similarity floor: cosine similarity >= 0.67
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
