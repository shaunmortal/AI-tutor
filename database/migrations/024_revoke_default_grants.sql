-- Newer Supabase projects give `anon` and `authenticated` TRUNCATE, REFERENCES and TRIGGER on
-- every new table by default. TRUNCATE ignores row-level security, so none of these belong to a
-- learner: every grant they need is made table by table in that table's own migration.
revoke truncate, references, trigger on all tables in schema public from anon, authenticated;
alter default privileges in schema public revoke truncate, references, trigger on tables from anon, authenticated;
