# Global engineering rules

These rules apply to every Praector task. Role-specific contracts may narrow their application, but may not silently weaken or discard them.

## Durable repository knowledge

When a substantive task performs repository file writes, do not leave important knowledge only in the implementation, task result, or agent session.

Before completing the task, inspect the relevant `README.md` and `AGENTS.md` files governing the affected repository area. If the work changes behavior, architecture, interfaces, developer workflow, operational assumptions, constraints, conventions, or other durable project knowledge, update the relevant `README.md` and/or `AGENTS.md` files so that knowledge is preserved.

Do not edit documentation merely for ceremony. If the applicable documentation remains accurate and no update is warranted, leave it unchanged and state in the result that it was checked.

## Code-change discipline

When working on a codebase or writing programming code:

- Prefer the smallest coherent change that fully satisfies the objective. Avoid unrelated cleanup, broad refactors, or speculative improvements.
- Before and after changing a symbol, consider its surrounding impact: variables it reads or mutates, callers and callees, shared state, control flow, error paths, tests, configuration, and other code whose assumptions may depend on the changed behavior.
- When changing code that participates in inheritance, interfaces, protocols, composition, or related class behavior, inspect the relevant parent, child, sibling, implementation, and consuming code. Ensure the surrounding contract remains valid and update affected code when required.
- If you identify worthwhile changes that are not required for the current objective, do not silently expand scope. Surface them as recommendations, risks, or questions so they can be explicitly accepted, deferred, or recorded.
- Prefer simple, direct plans and implementations over unnecessary abstraction or complexity. Use the least-complex change that satisfies the actual requirement and remains maintainable.
- Match the style, structure, naming, formatting, and idioms of the surrounding code. Do not make sweeping syntax, formatting, or stylistic changes unless they are necessary for the task or explicitly requested.
- Use docstrings wherever they add durable understanding, especially for new or materially changed public modules, classes, functions, methods, and non-obvious internal APIs. Follow the repository's existing documentation style and avoid docstrings that merely restate obvious code. For languages without docstrings, use the repository's equivalent documentation convention.

## Authoritative research-constraint precedence

When the Task Envelope contains `inherited_research_constraints`, those constraints are machine-projected from completed research lineage and are part of the effective task contract. They have higher precedence than conflicting paraphrases in the task objective, dependency summaries, critic prose, coordinator prose, or prior plan text.

- Preserve each canonical `planner_directive` exactly unless a durable explicit-exception decision is cited for that claim.
- If free-form task text conflicts with an inherited research constraint, do not follow the conflicting text. Treat the inherited constraint as authoritative, surface the conflict in the result, and avoid silently weakening or reversing it.
- Research constraints propagate through dependency lineage, so an implementation/review may inherit research even when its direct dependency is a plan review or implementation rather than the research Task itself.

## Delegation and planning

When creating a plan, durable task, or set of steps for another worker, preserve these global rules as part of that worker's context.

Praector automatically injects this global contract into every task envelope, so do not duplicate the entire contract in each task objective. Instead, include any task-specific implications in `DELIVERABLE`, `SCOPE`, `MUST HAVE`, `MUST NOT`, `VERIFY`, or `CONTEXT`.
