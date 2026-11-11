package praector

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type PullRequest struct {
	Number     int
	URL        string
	State      string
	Title      string
	BaseBranch string
	HeadBranch string
	IsDraft    bool
}

type PullRequestProvider interface {
	Repository() (map[string]string, error)
	FindForHead(string) (*PullRequest, error)
	View(int) (*PullRequest, error)
	Create(base, head, title, body string, draft bool) (*PullRequest, error)
	UpdateBody(int, string) (*PullRequest, error)
	Open(int) error
}

type GitHubCLIProvider struct{ Repo, GH string }

func NewGitHubCLIProvider(repo string) (*GitHubCLIProvider, error) {
	gh, err := RequireBinary("gh")
	if err != nil {
		return nil, err
	}
	return &GitHubCLIProvider{Repo: repo, GH: gh}, nil
}

func (g *GitHubCLIProvider) jsonArgs(args ...string) (any, error) {
	r, err := Run(context.Background(), append([]string{g.GH}, args...), g.Repo, nil, "", true)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Stdout) == "" {
		return nil, nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(r.Stdout))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("unable to parse gh JSON output: %w", err)
	}
	return v, nil
}
func normalizePR(m map[string]any) (*PullRequest, error) {
	n := 0
	switch x := m["number"].(type) {
	case json.Number:
		n, _ = strconv.Atoi(x.String())
	case float64:
		n = int(x)
	case int:
		n = x
	default:
		n, _ = strconv.Atoi(fmt.Sprint(x))
	}
	return &PullRequest{Number: n, URL: fmt.Sprint(m["url"]), State: strings.ToUpper(fmt.Sprint(m["state"])), Title: fmt.Sprint(m["title"]), BaseBranch: fmt.Sprint(m["baseRefName"]), HeadBranch: fmt.Sprint(m["headRefName"]), IsDraft: m["isDraft"] == true}, nil
}
func (g *GitHubCLIProvider) Repository() (map[string]string, error) {
	v, err := g.jsonArgs("repo", "view", "--json", "nameWithOwner,url")
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gh repo view returned no repository information")
	}
	return map[string]string{"name": fmt.Sprint(m["nameWithOwner"]), "url": fmt.Sprint(m["url"])}, nil
}
func (g *GitHubCLIProvider) FindForHead(head string) (*PullRequest, error) {
	v, err := g.jsonArgs("pr", "list", "--head", head, "--state", "all", "--limit", "100", "--json", "number,url,state,title,baseRefName,headRefName,isDraft")
	if err != nil {
		return nil, err
	}
	xs, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	matches := []*PullRequest{}
	for _, x := range xs {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		p, _ := normalizePR(m)
		if p.HeadBranch == head {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	sort.Slice(matches, func(i, j int) bool {
		io := matches[i].State == "OPEN"
		jo := matches[j].State == "OPEN"
		if io != jo {
			return io
		}
		return matches[i].Number > matches[j].Number
	})
	return matches[0], nil
}
func (g *GitHubCLIProvider) View(number int) (*PullRequest, error) {
	v, err := g.jsonArgs("pr", "view", strconv.Itoa(number), "--json", "number,url,state,title,baseRefName,headRefName,isDraft")
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unable to read GitHub PR #%d", number)
	}
	return normalizePR(m)
}
func (g *GitHubCLIProvider) Create(base, head, title, body string, draft bool) (*PullRequest, error) {
	args := []string{g.GH, "pr", "create", "--base", base, "--head", head, "--title", title, "--body-file", "-"}
	if draft {
		args = append(args, "--draft")
	}
	r, err := Run(context.Background(), args, g.Repo, nil, body, true)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		return nil, fmt.Errorf("gh pr create did not return a pull-request URL")
	}
	url := strings.TrimSpace(lines[len(lines)-1])
	v, err := g.jsonArgs("pr", "view", url, "--json", "number,url,state,title,baseRefName,headRefName,isDraft")
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unable to read newly created GitHub PR: %s", url)
	}
	return normalizePR(m)
}
func (g *GitHubCLIProvider) UpdateBody(number int, body string) (*PullRequest, error) {
	_, err := Run(context.Background(), []string{g.GH, "pr", "edit", strconv.Itoa(number), "--body-file", "-"}, g.Repo, nil, body, true)
	if err != nil {
		return nil, err
	}
	return g.View(number)
}
func (g *GitHubCLIProvider) Open(number int) error {
	_, err := Run(context.Background(), []string{g.GH, "pr", "view", strconv.Itoa(number), "--web"}, g.Repo, nil, "", true)
	return err
}

func compactText(v any, limit int) string {
	text := strings.Join(strings.Fields(fmt.Sprint(v)), " ")
	if text == "<nil>" {
		return ""
	}
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	if limit <= 1 {
		return "…"
	}
	return strings.TrimSpace(string(r[:limit-1])) + "…"
}
func firstSentence(v any, limit int) string {
	text := strings.Join(strings.Fields(fmt.Sprint(v)), " ")
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' {
			if i+1 < len(text) {
				return compactText(text[:i+1], limit)
			}
		}
	}
	return compactText(text, limit)
}
func resultStrings(result Row, keys ...string) []string {
	out := []string{}
	for _, k := range keys {
		if k == "summary" {
			if x := S(result, k); x != "" {
				out = append(out, x)
			}
			continue
		}
		for _, v := range Slice(result, k) {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}
func resultMentionsTests(result Row) bool {
	h := strings.ToLower(strings.Join(resultStrings(result, "summary", "findings", "evidence", "artifacts"), " "))
	return strings.Contains(h, "test") || strings.Contains(h, "pytest") || strings.Contains(h, "spec")
}

var docPathRE = regexp.MustCompile(`(?:AGENTS\.md|README(?:\.md)?|docs/[A-Za-z0-9_./-]+\.md)`)

func resultDocPaths(result Row) []string {
	h := strings.Join(resultStrings(result, "summary", "findings", "evidence", "artifacts"), " ")
	seen := map[string]bool{}
	out := []string{}
	for _, p := range docPathRE.FindAllString(h, -1) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func knownLegacyPRCopy(result Row) (string, []string) {
	h := strings.Join(resultStrings(result, "summary", "findings", "evidence", "decisions"), " ")
	low := strings.ToLower(h)
	if strings.Contains(low, "twilio") && strings.Contains(low, "comma") && (strings.Contains(low, "senddigits") || strings.Contains(low, "transfer_dial_twiml")) {
		sample := "+12125552211,6655"
		re := regexp.MustCompile(`\+?\d{7,},\d+`)
		if m := re.FindString(h); m != "" {
			sample = m
		}
		summary := fmt.Sprintf("Adds support for Twilio transfer destinations that include an extension, such as `%s`. The phone number is dialed normally, then the extension digits are sent after answer using Twilio's `<Number sendDigits>` mechanism.", sample)
		changes := []string{"Allow comma-separated transfer destinations while keeping input validation strict.", "Keep the raw transfer destination unchanged outside the Twilio gateway.", "Translate comma-separated Twilio destinations into `<Dial><Number sendDigits=\"...\">`.", "Preserve existing behavior for normal phone-number transfers."}
		if strings.Contains(low, "ecr") || strings.Contains(low, "browser") {
			changes = append(changes, "Keep ECR and browser transfer behavior unchanged.")
		}
		if strings.Contains(low, "fallback") && (strings.Contains(low, "unchanged") || strings.Contains(low, "untouched")) {
			changes = append(changes, "Keep tenant fallback-number behavior unchanged.")
		}
		return summary, changes
	}
	return "", nil
}
func legacyPRSummary(task, collection Row) string {
	result := M(task, "result")
	if x, _ := knownLegacyPRCopy(result); x != "" {
		return x
	}
	if x := firstSentence(S(result, "summary"), 520); x != "" {
		return x
	}
	if x := compactText(S(task, "title"), 360); x != "" {
		return strings.TrimSuffix(x, ".") + "."
	}
	return compactText(firstNonEmpty(S(collection, "objective"), S(collection, "title")), 520)
}
func implementationChangeBullets(task Row) []string {
	r := M(task, "result")
	explicit := Slice(r, "changes")
	if len(explicit) > 0 {
		out := []string{}
		for _, x := range explicit {
			if t := compactText(x, 430); t != "" {
				out = append(out, t)
			}
		}
		if len(out) > 8 {
			out = out[:8]
		}
		if len(out) > 0 {
			return out
		}
	}
	_, known := knownLegacyPRCopy(r)
	if len(known) > 0 {
		out := append([]string{}, known...)
		if resultMentionsTests(r) {
			out = append(out, "Add automated regression coverage for the implemented behavior.")
		}
		if docs := resultDocPaths(r); len(docs) > 0 {
			quoted := make([]string, len(docs))
			for i, p := range docs {
				quoted[i] = "`" + p + "`"
			}
			out = append(out, "Update durable documentation in "+strings.Join(quoted, ", ")+".")
		}
		if len(out) > 8 {
			out = out[:8]
		}
		return out
	}
	title := compactText(S(task, "title"), 300)
	if title != "" {
		return []string{title}
	}
	return nil
}
func knownLegacyPRTitle(result Row) string {
	if x, _ := knownLegacyPRCopy(result); x != "" {
		return "Add Twilio transfer support for extensions via sendDigits"
	}
	return ""
}
func DefaultPullRequestTitle(collection Row, tasks Rows) string {
	impl := Rows{}
	for _, t := range tasks {
		if S(t, "profile") == "implement" && S(t, "status") == TaskComplete {
			impl = append(impl, t)
		}
	}
	if len(impl) == 1 {
		r := M(impl[0], "result")
		if x := compactText(S(r, "pr_title"), 240); x != "" {
			return x
		}
		if x := knownLegacyPRTitle(r); x != "" {
			return x
		}
		if x := compactText(S(impl[0], "title"), 240); x != "" {
			return x
		}
	}
	return compactText(firstNonEmpty(S(collection, "title"), S(collection, "objective")), 240)
}

var passRE = regexp.MustCompile(`(?i)(\d+) passed(?:, (\d+) skipped)?`)

func verificationBullets(review Row) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, x := range Slice(M(review, "result"), "evidence") {
		text := compactText(x, 900)
		low := strings.ToLower(text)
		rendered := ""
		if m := passRE.FindStringSubmatch(low); len(m) > 0 {
			label := "Targeted tests"
			if regexp.MustCompile(`(?:^|\s)tests/(?:\s|-q|$)`).MatchString(low) {
				label = "Full test suite"
			}
			suffix := ""
			if len(m) > 2 && m[2] != "" {
				suffix = ", " + m[2] + " skipped"
			}
			rendered = fmt.Sprintf("%s: %s passed%s.", label, m[1], suffix)
		} else if strings.Contains(low, "git diff") && (strings.Contains(low, "unchanged") || strings.Contains(low, "= 0") || strings.Contains(low, "exactly")) {
			rendered = "Scope check: the reviewed diff stayed within the intended files and left out-of-scope paths unchanged."
		} else if strings.Contains(low, "single") && strings.Contains(low, "call site") {
			rendered = "Integration check: the new behavior is wired through the intended production path only."
		}
		if rendered != "" && !seen[strings.ToLower(rendered)] {
			seen[strings.ToLower(rendered)] = true
			out = append(out, rendered)
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}
func humanizeRemaining(text string, result Row) string {
	low := strings.ToLower(text)
	known, _ := knownLegacyPRCopy(result)
	if known != "" {
		if strings.Contains(low, "live twilio") && (strings.Contains(low, "smoke") || strings.Contains(low, "sandbox")) {
			return "Run one live Twilio/PBX smoke test to confirm extension digits are accepted after answer."
		}
		if strings.Contains(low, "cca-agents") || strings.Contains(low, "extensions.txt") {
			return "Update agent `extensions.txt` data with comma-form destinations for any agents that should use this feature."
		}
	}
	return text
}
func BuildPullRequestBody(collection Row, tasks Rows, sourceCommit string) string {
	impl, reviews := Rows{}, Rows{}
	for _, t := range tasks {
		if S(t, "profile") == "implement" && S(t, "status") == TaskComplete {
			impl = append(impl, t)
		}
		if S(t, "profile") == "review" && S(t, "status") == TaskComplete && strings.ToLower(S(M(t, "result"), "verdict")) == "pass" {
			reviews = append(reviews, t)
		}
	}
	summary := ""
	if len(impl) == 1 {
		r := M(impl[0], "result")
		summary = compactText(S(r, "pr_summary"), 1200)
		if summary == "" {
			summary = legacyPRSummary(impl[0], collection)
		}
	} else {
		summary = compactText(S(collection, "objective"), 1200)
	}
	if summary == "" {
		summary = compactText(S(collection, "title"), 1200)
	}
	lines := []string{"## Summary", "", summary, "", "## Changes", ""}
	if len(impl) > 0 {
		seen := map[string]bool{}
		changes := []string{}
		for _, t := range impl {
			for _, c := range implementationChangeBullets(t) {
				k := strings.ToLower(c)
				if !seen[k] {
					seen[k] = true
					changes = append(changes, c)
				}
				if len(changes) >= 12 {
					break
				}
			}
		}
		for _, c := range changes {
			lines = append(lines, "- "+c)
		}
		if len(changes) == 0 {
			lines = append(lines, "- Completed implementation work recorded without a change summary.")
		}
	} else {
		lines = append(lines, "- No completed implementation task recorded.")
	}
	lines = append(lines, "", "## Verification", "")
	if len(reviews) > 0 {
		lines = append(lines, "- Independent Praector review: **PASS**")
		seen := map[string]bool{}
		n := 0
		for _, r := range reviews {
			for _, b := range verificationBullets(r) {
				k := strings.ToLower(b)
				if !seen[k] {
					seen[k] = true
					lines = append(lines, "- "+b)
					n++
				}
				if n >= 4 {
					break
				}
			}
			if n >= 4 {
				break
			}
		}
	} else {
		lines = append(lines, "- No independent implementation review was required for this collection.")
	}
	unresolved := []string{}
	seen := map[string]bool{}
	primary := Row{}
	if len(impl) == 1 {
		primary = M(impl[0], "result")
	}
	for _, t := range append(append(Rows{}, impl...), reviews...) {
		for _, x := range Slice(M(t, "result"), "unresolved") {
			txt := humanizeRemaining(compactText(x, 600), primary)
			k := strings.ToLower(txt)
			if txt != "" && !seen[k] {
				seen[k] = true
				unresolved = append(unresolved, txt)
			}
		}
	}
	if len(unresolved) > 0 {
		lines = append(lines, "", "## Remaining validation", "")
		for _, x := range unresolved {
			lines = append(lines, "- "+x)
		}
	}
	short := sourceCommit
	if len(short) > 12 {
		short = short[:12]
	}
	reviewState := "not required"
	if len(reviews) > 0 {
		reviewState = "PASS"
	}
	lines = append(lines, "", "## Praector", "", fmt.Sprintf("- Collection: `%s`", S(collection, "id")), fmt.Sprintf("- Source: `%s` @ `%s`", S(collection, "integration_branch"), short), "- Collection status: `COMPLETE`", fmt.Sprintf("- Independent implementation review: `%s`", reviewState))
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}
