# Runtime agent protocol

[Documentation index](README.md) · [Architecture](ARCHITECTURE.md) · [CLI](CLI.md) · [Workspaces](WORKSPACES.md)

This document describes the contract between Praector and the disposable OpenCode sessions it launches. It is **not** a guide for coding agents modifying the Praector repository; that entry point is [`../AGENTS.md`](../AGENTS.md).

## Principle

> **Agents do not spawn agents. Agents request durable work.**

OpenCode's native `task`/background-agent mechanism is denied for Praector-managed runs. Delegation is represented as durable Tasks in the Praector graph.

## Profiles

Praector currently uses a small role taxonomy with a mandatory inherited global engineering contract plus dedicated role-specific prompt contracts. See [`PROFILES.md`](PROFILES.md) for the rationale, OMO analogue map, and prompt file locations.

### `coordinator`

Privileged planner/graph authority. It classifies the work, inspects context, resolves planning assumptions, decomposes the objective, creates explicit dependencies, and repairs the graph. It is read-only with respect to project files.

For complex/ambiguous work it can request a blocking `critic`. For architecture/tradeoff work it can request `advise`. For non-trivial plans it can create `plan_review` and gate write tasks on it.

In addition, `planning_policy` in the Task Envelope is mandatory. `explore_required`, `research_required`, `critic_required`, `plan_review_required`, `external_conformance_required`, and `threat_model_required` are scheduler-enforced policy, not coordinator preferences. Fresh research may run in parallel with exploration; the critic depends on both when research is required. For a cross-cutting objective the coordinator may also fan out a small number of additional exploration tasks with disjoint scopes, which the scheduler already parallelizes.

The risk-derived gates (`external_conformance_required`, `threat_model_required`, and the integration-sensitive `critic_required`) are triggered by the **detected risk category**, not by whether the Collection objective happens to match a mutation verb, so noun-phrase or passive objectives that genuinely touch security, an external protocol/contract/integration, or a network boundary receive the same gate as their imperative phrasing. Genuinely implementation-ceremony signals (the `plan_review` implementation term, `delivery_policy.implementation_required`, and the research freshness triggers) stay verb-based, and `explore_required` is `implementation OR any risk category recorded`. This de-verbing is forward-scoped by the collection `created_at` against `GateCoherenceCutoff`: pre-cutoff collections keep the historical verb-ANDed gates at every enforcement seam (including the claim gate and COMPLETE reconciliation), so no in-flight or already-completed work is retroactively re-gated. The `PLANNING_POLICY_ENFORCED` event records which derivation applied via its additive `gate_derivation` field.

The final `plan_review` gate over write work is an **artifact invariant of the graph, not only a `planning_policy` boolean**. In a coherent (post-`GateCoherenceCutoff`) Collection an implement is never claimable until its latest `plan_review` has reached a terminal **passing** decision (`COMPLETE` with `verdict: pass`), independent of what the objective-text classifier inferred, and the missing final review is auto-staged for a write-bearing Collection even when all four gates classified false (a `WAITING` coordinator, which has already handed off, does not suppress that staging). This closes the classifier blind spot where an objective matching no mutation verb or risk pattern (for example one referencing a CloudFormation parameter, whose `\bcloud\b` pattern never matches) previously let an implementation run before any plan-review decision. A claim held by this floor is recorded distinctly as a `PLAN_REVIEW_FLOOR_HELD` event rather than under the policy path; pre-cutoff collections keep the exact pre-floor behavior.

### `explore`

Read-only current-system discovery. It maps relevant code, tests, configuration, runtime boundaries, dependencies, conventions, and durable documentation before planning. It reports evidence and constraints but does not choose architecture or design the implementation. Implementation Collections receive this preflight automatically.

### `critic`

Read-only pre-planning critic. It identifies hidden intent, contradictions, scope inflation, missing prerequisites, unsafe assumptions, and weak QA/acceptance criteria. Its output is actionable directives for the coordinator rather than implementation.

### `plan_review`

Read-only practical reviewer of the proposed durable work graph. It checks references, task executability, dependency coherence, scope boundaries, and verification executability. It is blocker-focused and approval-biased. A fail verdict remains blocking and wakes a coordinator for graph repair; after repair the same durable plan-review Task runs again in a fresh Attempt.

### `research`

Read-only bounded evidence work. Deterministic fresh research uses current external retrieval, records `as_of`, structured `sources` and `claims`, normative level, claim-to-directive effect, confidence, and classified `verification_oracles`. Those fields are preserved in dependency envelopes and research result digests participate in material-plan staleness.

Critic, plan-review, review, and advise tasks that hit a decisive externally-verifiable uncertainty may submit `status: waiting` with `research_required`; Praector creates a blocking research child and reruns the same durable task after the evidence completes.

The same specialists may submit `status: waiting` with `explore_required` when the decisive gap is answerable from the repository itself; this `EXPLORE_REQUIRED` repository-internal counterpart creates a blocking read-only explore child and reruns the same durable task afterward. It has no capability gate, and only one of `research`/`explore` is used for a given uncertainty.


### `implement`

The only write-capable profile. It makes the bounded project-file change, runs relevant verification, and reports concrete evidence.

### `review`

Read-only post-implementation verifier. It independently inspects the integrated candidate, reruns appropriate checks/tests/smoke tests, and returns an explicit `pass`/`fail` verdict. This is intentionally distinct from `plan_review`.

### `advise`

Read-only strategic technical advisor. It reasons through difficult tradeoffs and returns one concrete recommendation with rationale, risks, uncertainty, and a revisit trigger.

## Built-in profile prompts

The common Task Envelope is generated by `internal/praector/envelope.go`. Every attempt first inherits the mandatory packaged global engineering contract, then its specialized role behavior:

```text
internal/assets/prompts/global.md
internal/assets/prompts/coordinator.md
internal/assets/prompts/critic.md
internal/assets/prompts/plan_review.md
internal/assets/prompts/research.md
internal/assets/prompts/implement.md
internal/assets/prompts/review.md
internal/assets/prompts/advise.md
```

Inspect the active built-in contracts with:

```bash
praector profile list
praector profile show coordinator
praector profile show explore
praector profile show critic
praector profile show plan_review
praector profile show advise
```

`praector profile show <profile>` prints the inherited global engineering contract first and the selected role-specific contract second.

`global.md` is not a profile. It is injected into every Task Envelope before the role-specific operating contract. It covers durable README/AGENTS knowledge preservation, minimal coherent edits, surrounding-impact and inheritance/interface analysis, scope restraint for adjacent improvements, simple implementation preference, local style preservation, and useful docstrings/documentation conventions. Delegated and retried workers therefore inherit the same rules automatically.

The role prompt files specialize reasoning behavior; deterministic authority still lives in Praector code/permissions/state checks. A prompt cannot grant capabilities that the orchestrator denies.

## Task Envelope

Every fresh Attempt receives a generated `PRAECTOR TASK ENVELOPE v1`.

The Envelope is the semantic continuity layer between disposable sessions. It includes:

- Project identity/name/base branch;
- assigned worktree and workspace mode;
- Collection identity/title/objective/status;
- Task identity/profile/title/objective/reason;
- Attempt identity/number;
- parent result summary;
- completed dependency result summaries;
- accepted Project/Collection memory;
- active decisions;
- inherited research constraints (when completed research lineage exists);
- inherited planning directives (when completed critic/plan-review/advise lineage produced them);
- the mandatory global engineering contract;
- role purpose;
- OWN / ASK / DELEGATE boundary;
- allowed durable delegation profiles;
- the mandatory role-specific operating contract;
- exact Praector commands for delegation/graph repair/escalation;
- structured result schema;
- STOP condition.

Agents should query current durable state with Praector commands rather than assuming old conversation context is authoritative. Coordinator, critic, and plan-review Envelopes also receive a snapshot of the current Collection task/edge graph.

## OWN / ASK / DELEGATE

The Envelope teaches every profile the same boundary model.

### OWN

Work that is inside this Task's responsibility and should be completed directly.

Examples:

- an implementer searching the relevant code and debugging its change;
- a researcher inspecting the repository for the requested evidence;
- a reviewer running tests against the integrated candidate.

### ASK

Durable context that can be fetched without creating another Task.

Useful commands:

```bash
praector context show
praector task show T-...
praector result show T-...
```

### DELEGATE

Independent or meaningfully out-of-scope work that deserves its own durable identity, result, retries, and lineage.

Typical reasons:

- a distinct specialist question;
- new implementation scope;
- a prerequisite discovered at runtime;
- independent verification;
- architecture judgment outside the current worker's authority;
- a newly discovered plan assumption that needs coordinator reconsideration.

## Request durable work

Non-blocking request:

```bash
praector task request \
  --profile research \
  --title "Inspect token revocation" \
  --objective "Determine how token revocation currently works, with code evidence" \
  --reason "The implementation depends on this behavior"
```

Blocking request:

```bash
praector task request \
  --profile advise \
  --title "Resolve storage choice" \
  --objective "Compare A and B and recommend one for this constraint" \
  --reason "Implementation cannot safely continue until this is resolved" \
  --blocking
```

Explicit dependencies may be attached, but every dependency must be the exact ID of an existing task in the same Collection. Capture or copy the IDs Praector actually returns; never invent symbolic `T-*` placeholders:

```bash
RESEARCH_TASK_ID="$(praector task request --profile research --title "Research SSO" --objective "Confirm the existing SSO constraints" --reason "Needed by implementation")"
ADVICE_TASK_ID="$(praector task request --profile advise --title "Choose SSO approach" --objective "Recommend the SSO approach under the known constraints" --reason "Needed by implementation")"

praector task request \
  --profile implement \
  --title "Implement SSO" \
  --objective "Implement the approved SSO flow with tests" \
  --reason "Approved collection plan" \
  --depends-on "$RESEARCH_TASK_ID" \
  --depends-on "$ADVICE_TASK_ID"
```

Praector rejects a dependency that does not resolve to a durable task or belongs to another Collection before publishing the new task/edge. This prevents dangling dependency rows from being silently ignored by scheduler joins.

Workspace choice can be requested with:

```text
--workspace auto|inherit|collection|isolated
```

`auto` is normally preferred. See [WORKSPACES.md](WORKSPACES.md#workspace-modes).

## Blocking semantics

A blocking request is not an instruction to keep the current session alive.

The intended flow is:

```text
Task Attempt RUNNING
   │
   ├── request blocking Child
   ├── Task -> WAITING
   └── current Attempt exits

Child completes
   │
   ▼
Task -> READY
   │
   ▼
fresh Attempt/session with Child result in Envelope
```

If the caller is an implementation Task and the delegated implementation needs the caller's in-progress code, Praector checkpoints the parent and uses inherited workspace semantics.

## Coordinator attention

A normal worker should not create another coordinator Task directly. If the graph/plan needs reconsideration:

```bash
praector coordinator attention \
  --reason "The plan assumes a schema invariant that is false" \
  --blocking
```

This preserves one privileged graph-authority boundary.

## Coordinator-only graph repair

A coordinator may add a dependency using exact task IDs:

```bash
praector task add-dependency "$TASK_ID" --on "$PREREQ_TASK_ID"
```

Or supersede failed/obsolete work:

```bash
praector task supersede T-OLD \
  --with T-NEW \
  --reason "The replacement handles the discovered constraint"
```

Supersession preserves lineage and rewires hard dependents from the old Task to the replacement.

## Structured result contract

Before ending, agents should submit structured JSON:

```bash
cat <<'JSON' | praector result submit --stdin
{
  "status": "complete",
  "summary": "Concise description of what was accomplished",
  "pr_summary": "Optional concise pull-request summary for implementation work",
  "changes": [],
  "findings": [],
  "evidence": [],
  "artifacts": [],
  "risks": [],
  "unresolved": [],
  "memory_candidates": [
    {
      "kind": "finding|constraint|decision|convention",
      "text": "Durable fact worth retaining"
    }
  ],
  "decisions": [
    {
      "statement": "Decision made",
      "rationale": "Why",
      "evidence": [],
      "alternatives": []
    }
  ]
}
JSON
```

Result shape is intentionally permissive in the current release, but `status`, `summary`, and the standard arrays make downstream inspection more reliable. For completed `implement` tasks, `pr_summary` should be 1–3 concise human-facing sentences and `changes` should contain 3–8 reviewer-facing behavior/scope bullets. `praector collection pr` prefers those fields verbatim; older implementation results without them fall back to durable findings/evidence.

If no structured result is submitted, Praector stores the final OpenCode response as a fallback summary for ordinary profiles. Every such legitimate fallback-text completion is marked `result_json.fallback_text_result = true` and emits a `RESULT_FALLBACK_TEXT` audit event (task+attempt scope) so reviews and traceability can mechanically distrust a transcript-style result; a worker-submitted structured result carries neither. Review and plan-review tasks are stricter: they must submit an explicit structured pass/fail verdict.

For the `implement` profile specifically, a silent zero-write completion is rejected by an artifact-derived phantom-write floor rather than auto-completed. If the exit-0 attempt produced no new commit versus the task's durable `base_commit` — measured by commit reachability (`git rev-list --count base_commit..HEAD`, never a working-tree diff and never the mutable `current_head` that `Checkpoint` overwrites each attempt) — and no structured result was submitted, the scheduler treats it as an execution/protocol failure. Within the attempt budget the same Task returns to `READY` with an `IMPLEMENT_PHANTOM_COMPLETION_REJECTED` event carrying a tail of the final text; the candidate worktree is reset to `base_commit` and `git clean -fd` removes untracked leftovers so a phantom attempt cannot leak into the next, and the checkpoint/integration tail is skipped so no no-op diff is integrated. On budget exhaustion it submits a failed result and the shared coordinator-attention tail fires. A structured result claiming completion is never mechanically phantom-flagged even at zero commits: the mechanical floor catches only silent completions, while semantic review catches dishonest structured ones.

### Review result

A completed review **must** include an explicit verdict:

```json
{
  "status": "complete",
  "verdict": "pass",
  "summary": "All acceptance criteria independently verified"
}
```

or:

```json
{
  "status": "complete",
  "verdict": "fail",
  "summary": "Blocking defect found"
}
```

A `fail` verdict drives an implementation repair Task rather than mutating code directly. The failed review then completes as durable historical evidence. The repair waits for that failed review to terminalize, and once the repair is integrated Praector creates a **fresh durable review Task/context**. The fresh reviewer independently re-checks the repaired candidate and is linked as verifier for both the repair implementation and the original implementation that failed review. The failed review is additionally a hard dependency of the fresh review, so its findings and evidence arrays flow into the new reviewer's `completed_dependencies` instead of being reduced to a one-line objective summary. Independence comes from re-running the checks against the repaired candidate — the fresh reviewer must re-check or explicitly refute each carried finding and state which; the prior verdict itself is not authoritative. A review created without a failed parent behaves exactly as before.

A passing review also carries a durable sibling-effect enumeration in `impact_enumeration`: the `method` used (literal-column grep set, call-graph walk, store-mutation sweep, ...) plus one `sites` entry per site that performs the same effect as the changed code (same state field/column write, same event emitter, same state-transition path, same contract caller), each with an `affected`/`unaffected` `disposition` and `evidence`. This makes the enumeration auditable and falsifiable by a human; it is not a completeness proof. A site that is a configuration key or flag introduced by the change must name its `reader_paths`; an introduced-but-never-read knob is a blocking defect, not a style preference. The rule is diff-scoped: pre-existing unused knobs keep the ordinary severity discipline. Praector additionally derives a heuristic candidate list of introduced knobs itself — `DeriveConfigCandidates` runs `git diff -z --name-only base...integration_branch` (the same three-dot merge-base basis as `CollectionDiff`, never a two-dot diff) and extracts setting-like tokens from added lines (flag literals, environment-variable names, config/table keys, setting-like struct/map keys; git's own flags/subcommands and well-known environment names are exempt). At the submit and acceptance seams the pure validators receive that list as data (no git access enters `policy.go`; claim/scheduler seams pass none and behave exactly as before), and a passing review must account for every candidate: name it in a site's identifier or `reader_paths`, or add an `impact_enumeration.candidate_dispositions` entry — `not-introduced`, or `deliberately-unread` with a durable decision id. Leaving the self-reported `introduced_knob` flag unset no longer opts out of the rule; it remains only a supplement. The candidate list is heuristic and human-falsifiable, not a completeness proof, and if integration introduces a new candidate after a review passed at submit, acceptance recomputes against the collection integration branch and correctly requires it (a genuine new knob, not diff drift). Impact enumeration is enforced at exactly two seams: result submission and collection acceptance.

When `inherited_directives` are present in the Envelope, a passing review (or plan review) must also carry `directive_dispositions` — see [Gate traceability](#gate-traceability-and-the-directive-ledger).

## Gate traceability and the directive ledger

The planning stages (critic, plan review, advise) emit directives — planner directives, QA directives, risk mitigations, and recommendations — inside their structured results. Those directives are durable but were previously checked by nothing. The gate-traceability layer closes that loop without adding any table:

- **Harvest.** `Store.DirectiveAncestors` (plus a lock-held variant) walks the same `depends_on` lineage as `ResearchAncestors` and collects, from completed critic/plan_review/advise ancestors, exactly: critic/plan-review `planner_directives`, critic/advise `qa_directives`, critic/advise `risks[].mitigation`, and advise `recommendations`. Research-claim planner directives are not harvested (they are governed by `research_consistency`), and advise tradeoffs/revisit-triggers/unresolved items are context, not directives.
- **Stable ids.** Each directive id is `<source-task-id>:<kind>:<ordinal-in-source-array>`, derived purely from persisted `result_json` order, so revalidation and reconciliation re-match prior dispositions and the ids cannot collide with research claim references (`<research-task-id>:<claim-id>`).
- **Projection.** Downstream Envelopes gain `inherited_directives` (id, kind, text, source task/profile, normative level) next to `inherited_research_constraints`, projected only when the set is non-empty.
- **Dispositions.** `ValidateDirectiveTraceability` — shaped like `ValidateResearchConsumerResult` — requires every passing review or plan review to disposition each inherited directive as `satisfied`, `deferred`, or `explicit_exception`; `deferred`/`explicit_exception` require a durable decision id from the existing decisions table (the same convention as `oracle_exception_decision_id`). Evidence is content-checked, not presence-checked: every disposition (and every impact site) must cite at least one token — a file path, a test name, or a command — and generic one-liners such as "verified in code" or "n/a" are rejected. Normative level is enforced, not only displayed: it is derived from the structured `normative_level` field and from a leading `MUST:`/`MUST NOT:`/`SHOULD:` prefix in plain-string directives (`MUST NOT` matched before `MUST`; only a leading prefix counts, and research-claim levels keep their separate structured path). A MUST-level directive may be `satisfied` or `explicit_exception` (with a durable decision id) but never `deferred`; SHOULD/unlevelled directives keep the three-way choice. Genuinely new directives belong in `additions` with a reason, never silently reported as satisfied coverage. Zero inherited directives means a clean pass with an empty set, and dispositions referencing unknown directive ids are still rejected. The directive validator is enforced at the same seams as research consistency — result submission, the claim-time planning barrier, and acceptance — and only for consumers: a critic is a producer and does not disposition its own directives. Impact enumeration by contrast is enforced at submission and acceptance only; there is deliberately no claim-time impact-enumeration gate (the former one was dead code and has been removed).
- **Legacy history.** Reviews and plan reviews completed before this contract are not retroactively failed unless the operator chooses enforcement; a single durable decision whose statement begins with `Legacy gate-traceability exception:` waives the retroactive read/acceptance gates — but only when that decision is collection-scoped to the checked collection (`collection_id` set, never project-wide or another collection), names that collection id in its statement, and carries no task scope (`task_id IS NULL`, the schema-free operator discriminator, because worker-created dispositions are always task-scoped). New submissions are always strict, and the derived-candidate accounting above is not waived even by a properly scoped legacy decision. See `docs/OPERATIONS.md`.
- **Silent-path observability.** Two previously fail-open conditions are now auditable rather than silent, and both add observability only — they never block work, manufacture busywork, or change a verdict. (1) An **absent diff basis** — a non-Git project root or an absent base/integration branch — must not read as a genuine no-knob diff. `DeriveConfigCandidates` (via a basis-aware variant) distinguishes three states: a git invocation error against an existing basis stays a hard error; an absent basis yields zero candidates **and** a named basis-unavailable outcome; a real derivable-but-empty diff stays silent. The submit seam and both acceptance seams record at most one deduped `CONFIG_BASIS_UNAVAILABLE` event per collection naming the missing ref, then continue with an empty candidate list. (2) A **zero directive harvest** — a completed review/plan_review whose directive-bearing critic/plan_review/advise ancestor yields zero harvestable directives (`len(ancestors) > 0` but `len(directives) == 0`) — is recorded once per consumer with a `DIRECTIVE_TRACEABILITY_EMPTY_HARVEST` event naming the consumer task and the checked source task ids. A genuinely directive-free lineage (`len(ancestors) == 0`, e.g. a review with no planning ancestor) stays a silent clean pass with no event. Because `Store.Event` re-enters the store lock, the empty-harvest event is emitted only at the two non-locked seams (the scheduler observer sweep and the acceptance CLI check); the submit store path and the claim-time directive gate are intentionally silent so a claim can never deadlock — the scheduler observer already covers submit-time consumers. Neither event is a completeness claim; both make an honest empty result distinguishable from a silent one. See `docs/OPERATIONS.md`.

`ValidateExternalConformanceReviewResult` is also tightened: an oracle exception (`oracle_exception_decision_id`) now additionally requires a non-empty `probes` list on the result, where each probe records the `scenario`, the `command_or_test` that exercised it, and the `observed_outcome`, so a documented exception cannot quietly discharge the strongest external gate. An internal live-loopback regression is an acceptable probe; an out-of-process same-interpretation client is still not authoritative conformance.

### Plan-review result

A completed `plan_review` also requires `verdict: pass|fail`. On fail, Praector keeps the plan reviewer blocking and creates coordinator-attention work. After the coordinator repairs/supersedes graph work, the plan reviewer becomes READY and re-evaluates the current graph in a fresh Attempt. Write tasks that depend on the plan-review gate remain blocked until it eventually passes.

Plan review must also reject a graph that knowingly instructs an implementation worker to violate a Collection MUST/MUST NOT or acceptance requirement. "A later review will catch it" is not a valid mitigation. Praector currently has no durable fault-injection/exception authorization, so such an intentional contradiction must block planning rather than release write work.

When critic policy applies, Praector also tracks critic freshness against a material-plan fingerprint. The initial critic is sealed at the first final plan-review handoff. Later changes to implementation/research/advice contracts or their meaningful dependencies make that critic stale. A stale critic prevents plan-review and implementation claims; Praector runs a fresh critic and then fresh coordinator incorporation before the plan reviewer can run again. If incorporation itself materially changes the plan, another critic round is required. Final review remains single-flight across those revalidation rounds: a coordinator reuses an existing non-failed nonterminal `plan_review`, `WAITING` coordinators are not fed back as review prerequisites, and an existing active-coordinator prerequisite is atomically flipped into the blocking handoff direction when final review is requested.

## Result lifecycle

Submitting a result does not immediately make dependent work runnable.

```text
agent calls `praector result submit`
          │
          ▼
Task -> VERIFYING
          │
          ├── OpenCode process must exit
          ├── capability/Git checks run
          ├── implementation checkpoint/integration runs
          ├── review setup may run
          ▼
Task -> COMPLETE / WAITING / FAILED
```

This prevents a still-running model process from racing downstream work.

## Memory candidates and decisions

Workers may propose `memory_candidates`. Praector stores them as candidates; Praector does not automatically promote every candidate to accepted project truth.

Workers may also return structured `decisions`. The scheduler persists valid decision records into the durable decision table.

Manual accepted memory can be added by an operator:

```bash
praector memory add \
  --scope project \
  --state accepted \
  --kind constraint \
  --text "Never put PHI in logs"
```

## Capability enforcement

The protocol is enforced in multiple layers.

### OpenCode permission injection

Managed runs deny:

- native `task` delegation;
- interactive user questions;
- external-directory access;
- `git push`.

Read-only profiles also deny edit tools.

### Read-only workspace hygiene

Every read-only Task Envelope makes no-net-workspace-mutation part of the completion contract. The worker captures its starting Git status, prefers verification modes that avoid incidental repository writes, and checks the worktree again before result submission. When tests or tooling create caches, coverage output, bytecode, generated files, or other incidental artifacts, the worker may restore only the changes caused by its own verification. It must never discard pre-existing changes or committed candidate content just to make the worktree clean.

For ordinary Python verification where bytecode caches are irrelevant, `PYTHONDONTWRITEBYTECODE=1` is one useful example; other ecosystems should use their own non-mutating/cache-redirection controls when practical. This is guidance, not a universal environment override, so verification semantics remain under the worker's control.

### Git validation

After read-only runs, Praector still checks the worktree deterministically. If a read-only worker leaves repository mutations behind through shell or another path, Praector rolls the changes back and treats the violation as failure. Prompt-level prevention reduces avoidable retries; runtime enforcement remains the final safety boundary.

### Workspace root

Every Envelope states that `PRAECTOR_WORKTREE` / `workspace.assigned_worktree` is the execution root. OpenCode is also launched with `--dir <assigned-worktree>`.

### Environment isolation

Praector removes an inherited active Python virtualenv activation from the worker environment before launching OpenCode, preventing project `python`/`pip` commands from accidentally resolving to the operator shell's environment. The native Praector executable itself has no Python runtime dependency. Explicit task environment overrides remain possible.

## Stop condition

A worker should stop when either:

- its bounded objective/acceptance boundary is satisfied and result submitted; or
- it has created a blocking durable Task for work it must not or should not perform itself.

The worker should not remain alive waiting for another Task to finish.

### Atomic task publication

A requested task is not scheduler-visible until its initial graph contract is durable. Praector writes the task row, initial `depends_on` edges, `spawned_by` edge, blocking child-to-parent edge, and parent `WAITING` transition under one state lock and semantic commit. This prevents a concurrent scheduler from claiming a task in the gap between task creation and dependency attachment. Coordinator-requested `plan_review` is always a blocking handoff and automatically carries the latest non-superseded critic as a dependency when one exists.


### Claim-time planning barrier

For collections whose deterministic planning policy requires plan review, implementation readiness is not determined only by the presence or absence of dependency rows during scheduler reconciliation. `claim_task()` independently refuses an `implement` task unless the latest valid plan review is complete, newer than the implementation contract, explicitly gates that implementation, includes any required critic, the latest critic's sealed material-plan fingerprint still matches, and no coordinator remains active. `plan_review` claims are also refused when the required critic is stale. Coordinator-requested plan review atomically gates existing implementation tasks. While a coordinator is still active, policy defers automatic fallback plan-review creation so the coordinator remains the sole owner of the final review handoff. On a passing final blocking plan review, Praector finalizes the waiting coordinator automatically; only failed review repair or material critic revalidation wakes planning again.

### Plan-review dependency direction

`plan_review` is a pre-write gate over durable implementation contracts. It may wait for prerequisite evidence (`explore`, `critic`, `research`, `advise`) but must not depend on `implement` execution. The valid direction is `plan_review -> implement` (implementation depends on the passing plan review). Praector strips coordinator-requested implementation prerequisites from plan review and repairs the exact reciprocal deadlock if encountered in an existing collection.
