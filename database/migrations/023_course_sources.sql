-- Course material a learner uploads: PDFs, slide decks and lecture videos.
--
-- Every unit of content keeps where it came from (page, slide, or video seconds) so the tutor can
-- cite it and the citation can open the exact spot. Search is Postgres full-text: no embedding
-- service, no extra key, and the same query runs locally and in production.
--
-- ponytail: lexical search only. Good on course material, where the learner's words and the
-- book's words overlap; add pgvector embeddings if paraphrased questions start missing.

create table if not exists public.course_sources (
    id uuid primary key default gen_random_uuid(),
    user_id uuid not null references public.profiles (id) on delete cascade,
    curriculum_id uuid references public.curricula (id) on delete set null,
    kind text not null,
    title text not null,
    -- Object path in the course-sources bucket, for uploaded files.
    storage_path text,
    -- The original video address, for lectures.
    video_url text,
    status text not null default 'processing',
    error text,
    created_at timestamptz not null default now(),

    constraint course_sources_kind_known check (kind in ('pdf', 'slides', 'video')),
    constraint course_sources_status_known check (status in ('processing', 'ready', 'failed')),
    constraint course_sources_title_length check (char_length(trim(title)) between 1 and 200),
    constraint course_sources_has_origin check (storage_path is not null or video_url is not null)
);

create index if not exists course_sources_user_idx
    on public.course_sources (user_id, created_at desc);

create table if not exists public.source_chunks (
    id uuid primary key default gen_random_uuid(),
    source_id uuid not null references public.course_sources (id) on delete cascade,
    -- Copied from the source so search filters on one indexed column without a join.
    user_id uuid not null references public.profiles (id) on delete cascade,
    topic_id uuid references public.topics (id) on delete set null,
    modality text not null,
    page integer,
    slide integer,
    start_sec integer,
    end_sec integer,
    content text not null,
    tsv tsvector generated always as (to_tsvector('english', content)) stored,

    constraint source_chunks_modality_known
        check (modality in ('text', 'figure', 'transcript')),
    constraint source_chunks_has_location
        check (page is not null or slide is not null or start_sec is not null),
    constraint source_chunks_content_not_blank check (char_length(trim(content)) > 0)
);

create index if not exists source_chunks_tsv_idx on public.source_chunks using gin (tsv);
create index if not exists source_chunks_user_idx on public.source_chunks (user_id);
create index if not exists source_chunks_topic_idx on public.source_chunks (topic_id);

-- Concepts and prerequisites extracted from the material.
alter table public.topics
    add column if not exists key_concepts text[] not null default '{}';

create table if not exists public.topic_prerequisites (
    topic_id uuid not null references public.topics (id) on delete cascade,
    prerequisite_id uuid not null references public.topics (id) on delete cascade,
    primary key (topic_id, prerequisite_id),
    constraint topic_prerequisites_not_self check (topic_id <> prerequisite_id)
);

-- A question written from the material remembers the excerpt it came from.
alter table public.questions
    add column if not exists source_chunk_id uuid
        references public.source_chunks (id) on delete set null;

-- Tutor replies keep the excerpts they cited.
alter table public.ai_messages
    add column if not exists citations jsonb;

/**
 * Ranked excerpts for one learner.
 *
 * The question's words are OR-ed rather than AND-ed: a learner's question rarely uses every word
 * the book does, and an AND query returns nothing for most real questions. ts_rank_cd still puts
 * excerpts that match more of the words first.
 */
create or replace function public.search_source_chunks(
    p_user_id uuid,
    p_query text,
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
set search_path = public, pg_temp
as $$
    with q as (
        select nullif(replace(plainto_tsquery('english', p_query)::text, '&', '|'), '')::tsquery
            as query
    )
    select c.id, c.source_id, c.topic_id, c.modality, c.page, c.slide, c.start_sec, c.end_sec,
           c.content, ts_rank_cd(c.tsv, q.query) as rank
    from public.source_chunks c, q
    where c.user_id = p_user_id
      and q.query is not null
      and c.tsv @@ q.query
    order by rank desc
    limit greatest(1, least(p_limit, 20));
$$;

revoke all on function public.search_source_chunks(uuid, text, integer) from public, anon, authenticated;

alter table public.course_sources enable row level security;
alter table public.source_chunks enable row level security;
alter table public.topic_prerequisites enable row level security;

drop policy if exists "Learners read their own sources" on public.course_sources;
create policy "Learners read their own sources"
    on public.course_sources for select to authenticated
    using (auth.uid() = user_id);

drop policy if exists "Learners read their own excerpts" on public.source_chunks;
create policy "Learners read their own excerpts"
    on public.source_chunks for select to authenticated
    using (auth.uid() = user_id);

drop policy if exists "Signed-in learners read prerequisites" on public.topic_prerequisites;
create policy "Signed-in learners read prerequisites"
    on public.topic_prerequisites for select to authenticated
    using (true);

grant select on public.course_sources, public.topic_prerequisites to authenticated;
grant select (id, source_id, topic_id, modality, page, slide, start_sec, end_sec, content)
    on public.source_chunks to authenticated;

-- Private bucket: files are only ever read through short-lived signed URLs the API hands out.
insert into storage.buckets (id, name, public)
values ('course-sources', 'course-sources', false)
on conflict (id) do nothing;

-- Learners may see which excerpt a question came from (not its answer).
grant select (source_chunk_id) on public.questions to authenticated;
