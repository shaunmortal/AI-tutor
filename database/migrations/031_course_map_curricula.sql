-- 031_course_map_curricula.sql
--
-- A course map (POST /sources/organize) is stored as a curriculum, but unlike a resolved
-- syllabus it is built from one learner's own material, so it is not shared. This flag lets
-- GET /sources/map/{curriculumId} tell the two apart: a course map is readable only by a learner
-- whose sources are attached to it or whose goal points at it; a shared syllabus by anyone.
--
-- Before this flag, course maps were the only curricula with this description, written by
-- web/content/coursemap.go; the backfill relies on that.

alter table public.curricula
    add column if not exists is_course_map boolean not null default false;

update public.curricula c
   set is_course_map = true
 where not c.is_course_map
   and (c.description like 'Built from % uploaded source%'
        or exists (select 1 from public.course_sources s where s.curriculum_id = c.id));
