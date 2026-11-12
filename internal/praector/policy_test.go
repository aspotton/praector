package praector

import "testing"

func TestPlanningPolicyCompatibility(t *testing.T) {
	p := ClassifyPlanningPolicy("Add API-key authentication and per-key rate limiting to the public API endpoint.")
	if !p.ExploreRequired || !p.CriticRequired || !p.PlanReviewRequired {
		t.Fatalf("expected explore, critic, and plan-review gates: %#v", p)
	}
	has := func(x string) bool {
		for _, v := range p.Categories {
			if v == x {
				return true
			}
		}
		return false
	}
	if !has("security") || !has("stateful_behavior") {
		t.Fatalf("missing categories: %v", p.Categories)
	}
	p = ClassifyPlanningPolicy("Fix a typo in README.md and update one example sentence.")
	if !p.ExploreRequired || !p.CriticRequired || p.PlanReviewRequired {
		t.Fatalf("trivial implementation should explore + critique but skip plan review: %#v", p)
	}
}
func TestTwilioConversationalObjectiveCompatibility(t *testing.T) {
	obj := "In the Twilio gateway we make use of templates. I'd like to add a new feature, likely a new template, that when we initiate a transfer to a phone number and it has commas like +12125552211,6655 it would initial a call transfer much like it does now using the Dial verb. It would need to dial these other digits, 6655 in this case, at answer. This is how we will support transferring calls in Twilio to other internal extensions.\n\nThis only applies to bots that make use of Twilio."
	d := ClassifyDeliveryPolicy(obj)
	p := ClassifyPlanningPolicy(obj)
	if !d.ImplementationRequired {
		t.Fatal("Twilio conversational objective must require implementation")
	}
	if !p.CriticRequired || !p.PlanReviewRequired {
		t.Fatalf("Twilio integration must require critic + plan review: %#v", p)
	}
}
func TestMaterialPlanFingerprintMatchesPythonReference(t *testing.T) {
	objective := "Add API-key authentication and per-key rate limiting."
	tasks := Rows{
		{"id": "T-C", "profile": "critic", "title": "Critic", "objective": "Critique", "reason": "policy", "workspace_mode": "collection", "base_commit": "aaa"},
		{"id": "T-I", "profile": "implement", "title": "Implement", "objective": "Do work", "reason": "plan", "workspace_mode": "collection", "base_commit": "bbb"},
		{"id": "T-X", "profile": "implement", "title": "Child", "objective": "Child work", "reason": "delegate", "workspace_mode": "inherit", "base_commit": "ccc"},
		{"id": "T-R", "profile": "review", "title": "Review", "objective": "Review", "reason": "auto", "workspace_mode": "collection", "base_commit": "ddd"},
	}
	edges := Rows{{"from_task_id": "T-I", "to_task_id": "T-X", "relation": "depends_on"}, {"from_task_id": "T-C", "to_task_id": "T-I", "relation": "spawned_by"}, {"from_task_id": "T-I", "to_task_id": "T-R", "relation": "verifies"}}
	if got := MaterialPlanFingerprint(objective, tasks, edges, false); got != "53c05b0a57e14d7c689bf2230ff9a1625ce699dfb78f3a5288979b8866887e25" {
		t.Fatalf("canonical fingerprint mismatch: %s", got)
	}
	if got := MaterialPlanFingerprint(objective, tasks, edges, true); got != "22fb8265d5ecbeb3aa3209042dc6698084c4f2e400a443049b3aa9ba8be86c12" {
		t.Fatalf("legacy fingerprint mismatch: %s", got)
	}
}
