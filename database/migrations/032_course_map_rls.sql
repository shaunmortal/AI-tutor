-- Course maps are private to their learner, in the database as well as the API.
--
-- topics, curricula and topic_prerequisites were readable by every signed-in learner (using
-- (true)), which was right while every curriculum was a shared syllabus. A course map is built
-- from one learner's own material, so its topic names are theirs: the same rule the API applies
-- in GET /sources/map now holds for anybody querying the tables directly.
--
-- can_read_curriculum is SECURITY DEFINER so the policy can look at course_sources and
-- learning_goals without recursing through their own policies; it only ever answers yes/no for
-- the caller (auth.uid()), never returns rows.

create or replace function public.can_read_curriculum(p_curriculum_id uuid)
returns boolean
language sql
stable
security definer
set search_path = public
as $$
    select exists (
        select 1 from public.curricula c
        where c.id = p_curriculum_id
          and (
                not c.is_course_map
             or exists (select 1 from public.course_sources s
                        where s.curriculum_id = c.id and s.user_id = auth.uid())
             or exists (select 1 from public.learning_goals g
                        where g.curriculum_id = c.id and g.user_id = auth.uid())
          )
    );
$$;

revoke all on function public.can_read_curriculum(uuid) from public, anon;
grant execute on function public.can_read_curriculum(uuid) to authenticated;

drop policy if exists "Signed-in learners read curricula" on public.curricula;
create policy "Signed-in learners read curricula"
    on public.curricula for select to authenticated
    using (public.can_read_curriculum(id));

drop policy if exists "Signed-in learners read the syllabus" on public.topics;
create policy "Signed-in learners read the syllabus"
    on public.topics for select to authenticated
    using (public.can_read_curriculum(curriculum_id));

drop policy if exists "Signed-in learners read prerequisites" on public.topic_prerequisites;
create policy "Signed-in learners read prerequisites"
    on public.topic_prerequisites for select to authenticated
    using (exists (select 1 from public.topics t
                   where t.id = topic_id and public.can_read_curriculum(t.curriculum_id)));
