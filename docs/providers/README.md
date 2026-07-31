# Providers

Each provider is an adapter implementing [`sdk.Provider`](../../sdk/README.md). This directory documents, per provider: what it can do, how to configure it, the **minimum credential scope** it needs, its failure modes, and how to intervene manually when the platform cannot.

That last section exists because during an incident someone will have to touch the target directly, and the runbook needs somewhere to point.

## Registry

| Provider | Status | Rollback | Log streaming | Zero-downtime | Doc |
|---|---|---|---|---|---|
| Netlify | Planned V1 | ✅ (instant, by deploy ID) | ✅ | ✅ (atomic swap) | [netlify.md](netlify.md) |
| VPS / SSH | Planned V1 | ✅ (symlink swap) | ✅ | ⚠️ (with a supervisor) | [ssh.md](ssh.md) |
| Heroku | Planned V1 | ✅ (release rollback) | ✅ | ✅ | [heroku.md](heroku.md) |
| Docker | Planned V3 | ✅ (previous image tag) | ✅ | ⚠️ | — |
| Kubernetes | Planned V3 | ✅ (`rollout undo`) | ✅ | ✅ | — |
| AWS / Azure / GCP | Planned V4 | Varies | Varies | Varies | — |

## Least privilege, seriously

Every document here specifies the minimum permission set, because "give it admin" is how platforms become breaches. A Veyronix credential should be able to deploy the things it deploys and nothing else — in particular it should not be able to delete the account, read billing, or manage other users.

Where a provider's permission model is too coarse to express this (several are), the document says so explicitly rather than pretending otherwise. Knowing you are over-privileged is worth more than assuming you are not.

## Capability honesty

`Capabilities()` is a contract, not a marketing surface. A provider that declares canary support must actually perform a canary. If a capability cannot be implemented properly, declare `false` — the dashboard hides the control and the engine rejects the strategy at `Validate` time, which is a far better outcome than a "canary" that is silently a full replacement.

## Writing a new provider

1. Read [`sdk/README.md`](../../sdk/README.md).
2. Implement the interface.
3. Pass `sdk/conformance` — idempotency, error classification, log termination, rollback correctness.
4. Write a document here, following the structure of [netlify.md](netlify.md).
5. Register it in `internal/providers/registry`.

Steps 3 and 4 are not optional. A provider that passes conformance but has no operational documentation cannot be supported at 02:00.
