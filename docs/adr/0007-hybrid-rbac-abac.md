# ADR-0007 — Hybrid RBAC + ABAC authorization

- **Status:** Accepted
- **Date:** 2026-07-24
- **Deciders:** @nickemma

## Context

Deployment permissions have two independent dimensions. *What kind of action* — deploying is different from managing members, which is different from reading logs. And *on which resource* — a developer on the Payments team may deploy the Payments API and must not deploy the HR API.

Collapsing those into one mechanism goes badly in both directions. Pure roles produce `payments-developer`, `payments-lead`, `hr-developer`, `hr-lead`, `platform-developer` — a role per team per level, growing multiplicatively, each one a copy-paste of another with one field changed. Pure attributes produce a policy language that is technically expressive and practically unauditable; nobody can answer "what can Alice do?" without running the evaluator.

There is a third requirement, learned from every authorization system that gets disabled in production: decisions must be **explainable**. "Access denied" with no reason is a support ticket at best and a `--force` flag at worst.

## Decision

**Hybrid.** Roles carry action classes; attributes carry resource scope.

```
role: developer   → {deploy, rollback, read_logs, read_history}
role: lead        → developer + {approve, manage_env_vars}
role: admin       → lead + {manage_members, manage_targets, manage_secrets}
role: viewer      → {read_history, read_logs}
```

```
policy: subject.team == resource.project.team
policy: resource.environment.requires_approval == false OR subject.has(approve)
```

Evaluation is deny-by-default: a subject with no matching grant can do nothing. A decision is `ALLOW` only if some policy matches and no explicit deny applies.

Every decision is recorded, and `veyronix authz explain --subject alice@acme.io --action deploy --resource project/ecommerce-api/production` returns the matched policy, the attribute comparison performed, and the outcome.

## Alternatives Considered

**Pure RBAC.** Rejected: role explosion. The role count becomes teams × levels, and every new team means new roles rather than new attribute values.

**Pure ABAC.** Rejected: expressive but unauditable. Answering "who can deploy to production?" requires evaluating every subject against every policy instead of reading a list.

**OPA / Rego as the policy engine.** Genuinely good — battle-tested, expressive, decoupled. Rejected for V1: it adds a second language and a runtime dependency, and Rego's learning curve is real. The policy surface here is small and well understood. Reconsider if policies become complex enough that a bespoke evaluator is being reinvented badly — that is the signal, and it should be taken seriously when it appears.

**Google Zanzibar-style relationship tuples (SpiceDB, OpenFGA).** The right model for deeply nested resource hierarchies and sharing graphs. Rejected: Veyronix's hierarchy is shallow (org → team → project → environment) and the operational cost of another stateful service is not justified by it.

## Consequences

**Positive**

- Four roles cover the action space; team growth adds attribute values, not roles.
- One policy expresses "developers deploy their own team's projects" rather than one role definition per team.
- Explainable decisions mean the authorization system gets debugged instead of bypassed.
- Deny-by-default means a misconfiguration fails closed.

**Negative**

- Two mechanisms to reason about — a genuine conceptual cost for anyone reading the code for the first time.
- Attribute evaluation is on the hot path of every request; it must be cached carefully, and cache invalidation on membership change is a real problem, not a footnote.
- Policies stored as data need their own migration and review story. A bad policy deploy is an outage or a breach.

## Revisit If

- Policy count or nesting grows past what a bespoke evaluator handles cleanly → OPA.
- Resource sharing becomes graph-shaped (cross-team grants, delegation, inherited access) → Zanzibar-style.
