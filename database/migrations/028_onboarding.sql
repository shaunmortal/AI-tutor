-- 028_onboarding.sql
--
-- What the first-run intake asks for that had nowhere to live.
--
-- preferred_language: the language the tutor explains in. A learner who studies in Hinglish
-- should not have to type in Hinglish to be answered in it. Learners set it themselves, so it is
-- added to their column grant on profiles.
--
-- self_ratings: how confident the learner says they are in each area (a subject) before any
-- evidence exists. Used only as the mastery prior for topics with no attempts — the first real
-- answer replaces it — so a modest "I'm good at this" moves the plan a little and can never
-- claim a topic is mastered. Mapping lives in learner.SelfRatedPrior.

alter table public.profiles
    add column if not exists preferred_language text not null default 'english';

alter table public.profiles drop constraint if exists profiles_preferred_language_known;
alter table public.profiles add constraint profiles_preferred_language_known
    check (preferred_language in ('english', 'hindi', 'hinglish'));

grant update (preferred_language) on public.profiles to authenticated;

create table if not exists public.self_ratings (
    user_id uuid not null references public.profiles (id) on delete cascade,
    -- A subject (or a topic): a rating on a subject applies to every topic beneath it.
    topic_id uuid not null references public.topics (id) on delete cascade,
    confidence smallint not null,
    updated_at timestamptz not null default now(),
    primary key (user_id, topic_id),
    constraint self_ratings_confidence_range check (confidence between 1 and 5)
);

comment on table public.self_ratings is
    'Self-rated confidence (1-5) per area from onboarding. A prior for unmeasured topics only.';

drop trigger if exists self_ratings_set_updated_at on public.self_ratings;
create trigger self_ratings_set_updated_at
    before update on public.self_ratings
    for each row
    execute function extensions.moddatetime (updated_at);

alter table public.self_ratings enable row level security;
revoke all on public.self_ratings from anon, authenticated;

drop policy if exists "Learners read their own self ratings" on public.self_ratings;
create policy "Learners read their own self ratings"
    on public.self_ratings for select to authenticated
    using (auth.uid() = user_id);

grant select on public.self_ratings to authenticated;
