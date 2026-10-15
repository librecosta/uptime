# Architecture Decision Records

- [0001: Loopback defaults and first access](0001-local-first-startup.md) — Accepted.

ADRs preserve the context, decision, consequences, and rejected alternatives of
significant architectural changes so later contributors can understand why a
boundary exists.

`docs/design/v0.1.0-product-technical-design.md` is the initial architecture
baseline. Do not create retrospective ADRs merely to duplicate decisions already
captured there.

Create an ADR for a material boundary or compatibility change, a new long-term
constraint, or an expensive-to-reverse choice with plausible alternatives.
Routine file organization, ordinary bug fixes, and changes already explained by
the baseline do not need an ADR.

Use `NNNN-short-title.md`, beginning with `0001` for the first actual new decision.
Include a title, date, status, context, decision, and consequences. Statuses
include **Proposed**, **Accepted**, and **Superseded**. Link a superseding decision
and preserve the historical record rather than rewriting its rationale to match
the present. Update affected contracts and tests when a decision is implemented.
