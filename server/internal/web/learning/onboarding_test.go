package learning

import (
	"context"
	"testing"

	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

func TestOnboardingProfileAndConfidence(t *testing.T) {
	e := setup(t)
	u := e.u1

	r := e.call(u, "GET", "/learner/profile", "")
	check(t, r.Data["profile"].(map[string]any)["preferredLanguage"] == "english", "default language %v", r.Data)
	e.expect(e.call(u, "PATCH", "/learner/profile", `{"preferredLanguage":"klingon"}`), 400, "INVALID_INPUT",
		"preferredLanguage: must be english, hindi or hinglish")
	r = e.call(u, "PATCH", "/learner/profile", `{"preferredLanguage":"hinglish"}`)
	e.expect(r, 200, "", "")
	check(t, r.Data["profile"].(map[string]any)["preferredLanguage"] == "hinglish", "language %v", r.Data)

	e.expect(e.call(u, "PUT", "/learner/confidence", `{"ratings":[]}`), 400, "INVALID_INPUT", "")
	e.expect(e.call(u, "PUT", "/learner/confidence", `{"ratings":[{"topicId":"`+e.subjectID+`","confidence":6}]}`), 400, "INVALID_INPUT", "")
	e.expect(e.call(u, "PUT", "/learner/confidence",
		`{"ratings":[{"topicId":"`+e.subjectID+`","confidence":2},{"topicId":"`+e.subjectID+`","confidence":3}]}`), 400, "INVALID_INPUT", "")
	e.expect(e.call(u, "PUT", "/learner/confidence", `{"ratings":[{"topicId":"`+e.subjectID+`","confidence":5}]}`), 200, "", "")
	r = e.call(u, "GET", "/learner/confidence", "")
	ratings := r.Data["ratings"].([]any)
	check(t, len(ratings) == 1 && ratings[0].(map[string]any)["confidence"] == 5.0, "ratings %v", ratings)
	// Another learner sees none of it.
	r = e.call(e.u2, "GET", "/learner/confidence", "")
	check(t, len(r.Data["ratings"].([]any)) == 0, "u2 ratings %v", r.Data)

	// A rating on a subject is the prior for its unmeasured topics, in the planner's input.
	topics, err := store.NewPlans(e.database.Admin).FindPlannableTopics(context.Background(), u, []string{e.subjectID})
	must(t, err)
	check(t, len(topics) > 0, "no topics")
	for _, tp := range topics {
		check(t, tp.MasteryScore == nil && tp.PriorMastery != nil && *tp.PriorMastery == 65, "prior for %s: %v", tp.Name, tp.PriorMastery)
	}
	topics, err = store.NewPlans(e.database.Admin).FindPlannableTopics(context.Background(), e.u2, []string{e.subjectID})
	must(t, err)
	check(t, topics[0].PriorMastery == nil, "u2 got u1's prior")
}
