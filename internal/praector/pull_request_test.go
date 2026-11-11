package praector

import (
	"strings"
	"testing"
)

func TestExplicitPRMetadata(t *testing.T) {
	c := Row{"id": "COL-1", "title": "Feature", "objective": "Feature", "integration_branch": "praector/collections/col-1"}
	tasks := Rows{{"profile": "implement", "status": "COMPLETE", "title": "Implementation log title", "result": Row{"pr_title": "Add the feature", "pr_summary": "Adds the feature for callers.", "changes": []any{"Allow the new behavior.", "Preserve old behavior."}, "unresolved": []any{"Smoke it."}}}, {"profile": "review", "status": "COMPLETE", "title": "Review", "result": Row{"verdict": "pass", "evidence": []any{"pytest tests/ -q -> 20 passed, 1 skipped"}}}}
	if got := DefaultPullRequestTitle(c, tasks); got != "Add the feature" {
		t.Fatalf("title %q", got)
	}
	body := BuildPullRequestBody(c, tasks, "abcdef123456789")
	for _, x := range []string{"Adds the feature for callers.", "- Allow the new behavior.", "Independent Praector review: **PASS**", "Full test suite: 20 passed, 1 skipped.", "Smoke it."} {
		if !strings.Contains(body, x) {
			t.Fatalf("missing %q in:\n%s", x, body)
		}
	}
}
func TestLegacyTwilioPRCopyCompatibility(t *testing.T) {
	c := Row{"id": "COL-5f7dda8d4c", "title": "Twilio transfer", "objective": "Twilio transfer", "integration_branch": "praector/collections/col-5f7dda8d4c"}
	impl := Row{"profile": "implement", "status": "COMPLETE", "title": "Twilio comma-transfer: dial extension via Number sendDigits", "result": Row{"summary": "Twilio-only comma transfer is implemented for +12125552211,6655 with sendDigits.", "findings": []any{"tools/call_transfer.py validation now accepts comma values. ECR/browser listeners are unchanged.", "The tenant fallback path is unchanged."}, "evidence": []any{"python -m pytest tests/ -q -> 1268 passed, 1 skipped", "git diff --name-only -> AGENTS.md, docs/features/fast-tools.md"}, "unresolved": []any{"Live Twilio sandbox smoke call for the comma transfer."}}}
	review := Row{"profile": "review", "status": "COMPLETE", "title": "Review", "result": Row{"verdict": "pass", "evidence": []any{"python -m pytest tests/unit/tools/test_call_transfer.py tests/unit/test_gateways_twilio.py -q -> 49 passed", "python -m pytest tests/ -q -> 1268 passed, 1 skipped", "git diff HEAD~1 HEAD --name-only -> exactly AGENTS.md; diff line count for gateways/ecr.py = 0"}}}
	tasks := Rows{impl, review}
	if got := DefaultPullRequestTitle(c, tasks); got != "Add Twilio transfer support for extensions via sendDigits" {
		t.Fatalf("title %q", got)
	}
	body := BuildPullRequestBody(c, tasks, "6b5f854d3503")
	for _, x := range []string{"Allow comma-separated transfer destinations while keeping input validation strict.", "Targeted tests: 49 passed.", "Full test suite: 1268 passed, 1 skipped.", "Run one live Twilio/PBX smoke test"} {
		if !strings.Contains(body, x) {
			t.Fatalf("missing %q:\n%s", x, body)
		}
	}
	for _, x := range []string{"_COMMA_SEPARATED_DIGITS_RE", "python -m pytest", "git diff HEAD~1"} {
		if strings.Contains(body, x) {
			t.Fatalf("raw log prose leaked %q", x)
		}
	}
}
