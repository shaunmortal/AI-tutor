-- 026_mistake_patterns.sql
--
-- Misconceptions: not "you got Kinematics wrong" but "you keep treating velocity as speed".
-- This is the table 009_mistake_patterns.sql was reserved for and never got.
--
-- Two sources feed it, both decided server side:
--   - a wrong mcq/msq answer: each wrong option carries the misconception that makes it tempting
--     (questions.option_misconceptions, written by the question generator), so picking it is
--     evidence of that misconception;
--   - a wrong short answer: the AI rubric grader names the misconception it saw, if any.
--
-- One row per (learner, topic, misconception), counted up each time it recurs.

-- Aligned with options: element i is why option i tempts a learner, '' for a correct option.
-- Deliberately NOT granted to `authenticated`: the blanks would say which options are right.
alter table public.questions add column if not exists option_misconceptions text[];

alter table public.questions drop constraint if exists questions_option_misconceptions_aligned;
alter table public.questions add constraint questions_option_misconceptions_aligned check (
    option_misconceptions is null or cardinality(option_misconceptions) = cardinality(options)
);

create table if not exists public.mistake_patterns (
    id uuid primary key default gen_random_uuid(),

    user_id uuid not null references public.profiles (id) on delete cascade,
    topic_id uuid not null references public.topics (id) on delete cascade,

    -- A short label, e.g. "confuses mass with weight".
    misconception text not null,

    -- The attempts that showed it, newest last, capped by the writer.
    evidence_attempt_ids uuid[] not null default '{}',

    occurrences integer not null default 1,
    first_seen_at timestamptz not null default now(),
    last_seen_at timestamptz not null default now(),

    constraint mistake_patterns_label_sane
        check (char_length(trim(misconception)) between 1 and 300),
    constraint mistake_patterns_occurrences_positive check (occurrences > 0)
);

comment on table public.mistake_patterns is
    'Recurring misconceptions per learner and topic, derived from graded attempts. Written by the backend only.';

-- Same idea worded with different case is one misconception.
create unique index if not exists mistake_patterns_user_topic_label_uniq
    on public.mistake_patterns (user_id, topic_id, lower(misconception));

-- "This learner's most frequent misconceptions" — the dashboard and the post-test report.
create index if not exists mistake_patterns_user_recent_idx
    on public.mistake_patterns (user_id, occurrences desc, last_seen_at desc);

-- The misconception a wrong answer in a test showed, kept for the post-test review.
alter table public.mock_test_questions add column if not exists misconception text;

-- ---------------------------------------------------------------------------
-- Security
-- ---------------------------------------------------------------------------
-- Read your own; nobody writes but the backend, for the same reason mastery is read-only:
-- a learner able to write it could erase the evidence the app teaches from.
alter table public.mistake_patterns enable row level security;

drop policy if exists "Learners read their own mistake patterns" on public.mistake_patterns;
create policy "Learners read their own mistake patterns"
    on public.mistake_patterns
    for select
    to authenticated
    using (auth.uid() = user_id);

grant select on public.mistake_patterns to authenticated;
