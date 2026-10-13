-- 025_short_answer_questions.sql
--
-- Short-answer questions: the learner types a word or a sentence, and the server marks it.
--
-- The answer key keeps the 003 rule: it lives in correct_answer, which `authenticated` holds no
-- privilege on, so a short question's accepted answers and rubric are as unreachable from the
-- browser as an mcq's option index. Shape:
--
--   short  {"answers": ["photosynthesis", ...], "rubric": "what a right answer must say"}
--
-- Marking is a normalised match against `answers` first, and only when that fails an AI grade
-- against the rubric (server/internal/web/practice/grade.go).

alter table public.questions drop constraint if exists questions_type_known;
alter table public.questions add constraint questions_type_known
    check (question_type in ('mcq', 'msq', 'numeric', 'short'));

-- Typed answers have no options.
alter table public.questions drop constraint if exists questions_options_match_type;
alter table public.questions add constraint questions_options_match_type check (
    (question_type in ('mcq', 'msq') and array_length(options, 1) >= 2)
    or (question_type in ('numeric', 'short') and options is null)
);

-- A short question with no accepted answer or no rubric cannot be marked at all.
alter table public.questions drop constraint if exists questions_short_has_key;
alter table public.questions add constraint questions_short_has_key check (
    question_type <> 'short'
    or (jsonb_typeof(correct_answer -> 'answers') = 'array'
        and jsonb_array_length(correct_answer -> 'answers') >= 1
        and jsonb_typeof(correct_answer -> 'rubric') = 'string')
);

-- A test whose topics the learner picked themselves, rather than the plan or the goal.
alter table public.mock_tests drop constraint if exists mock_tests_source_known;
alter table public.mock_tests add constraint mock_tests_source_known
    check (source in ('plan', 'goal', 'chosen'));

-- What the rubric grader said about a typed answer, kept for the post-test review. Written only at
-- marking, so the learner's existing SELECT on this table reveals nothing before submission.
alter table public.mock_test_questions add column if not exists feedback text;
