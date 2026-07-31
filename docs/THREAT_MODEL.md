# Veyronix — Threat Model

| | |
|---|---|
| **Status** | Living document |
| **Last updated** | 2026-07-28 |
| **Method** | STRIDE per trust boundary, with attacker-goal analysis |
| **Scope** | Control plane, worker, outbox relay, provider layer, dashboard |
| **Out of scope** | Physical security of the hosting environment; the security of the target infrastructure itself |

---

## 1. What an attacker wants

Veyronix is worth attacking for exactly one reason: it holds the credentials that deploy to every target the organization owns, and the ability to push arbitrary code to production.

| Goal | Value to attacker |
|---|---|
| Exfiltrate provider credentials | Direct access to every deployment target — the crown jewels |
| Deploy attacker-controlled code to production | Supply-chain compromise of everything the org runs |
| Escalate from developer to admin | Access to other teams' projects and secrets |
| Read another team's secrets | Lateral movement |
| Destroy or corrupt deployment history | Cover tracks; break audit and recovery |
| Deny service | Prevent incident response — deploys and rollbacks unavailable during an outage |

The second row is the one that matters most. A platform that can deploy to production is a platform that can compromise production, and every design decision below follows from taking that seriously.

---

## 2. Trust boundaries

```
   Internet
      │
 ┌────┴──────────────────────────────────────── B1 ── public edge
 │  Dashboard (browser)  ·  CLI  ·  CI webhooks
 └────┬──────────────────────────────────────────
      │  TLS, OAuth session / API key
 ┌────┴──────────────────────────────────────── B2 ── control plane
 │  API  →  authz  →  modules
 └────┬──────────────────────────────────────────
      │  in-process / mTLS once split
 ┌────┴──────────────────────────────────────── B3 ── data tier
 │  PostgreSQL  ·  NATS JetStream
 └────┬──────────────────────────────────────────
      │  lease claim, event consume
 ┌────┴──────────────────────────────────────── B4 ── worker
 │  Worker process (holds decrypted secrets in memory)
 └────┬──────────────────┬───────────────────────
      │                  │
 ┌────┴───────── B5 ┐ ┌──┴──────────────── B6 ──┐
 │ Build container  │ │ Provider APIs / SSH     │
 │ UNTRUSTED CODE   │ │ external, semi-trusted  │
 └──────────────────┘ └─────────────────────────┘
```

**B5 is the sharpest boundary in the system.** Everything inside it is code the platform did not write, running because a developer pushed a commit. It must be treated as hostile.

---

## 3. Threats by boundary

### B1 — Public edge

| ID | Threat | STRIDE | Mitigation | Residual |
|---|---|---|---|---|
| B1.1 | Session token theft (XSS, stolen cookie) | S | HttpOnly + SameSite=Strict cookies, short session TTL, CSP without `unsafe-inline`, re-auth for secret access | XSS in a dependency remains possible |
| B1.2 | Forged CI webhook triggering a deploy | S | HMAC signature over the raw body, replay window on timestamp, per-project webhook secret | Secret leaked from CI config |
| B1.3 | CSRF-triggered deploy | T | Connect requires `Content-Type: application/json` + custom header; SameSite cookies | Low |
| B1.4 | Credential stuffing on OAuth callback | S | No passwords stored at all; OAuth state parameter validated, PKCE | Compromise of the identity provider account |
| B1.5 | Enumeration of projects via error messages | I | Uniform 404 for unauthorized and non-existent resources | Timing side channels |
| B1.6 | Deploy-request flood | D | Per-subject and per-project rate limits, idempotency collapses duplicates, one in-flight deploy per environment | Distributed flood on the edge |

### B2 — Control plane

| ID | Threat | STRIDE | Mitigation | Residual |
|---|---|---|---|---|
| B2.1 | Privilege escalation by self-granting a role | E | Role changes require `manage_members`; no subject may elevate its own grants; every change audited | Compromise of an existing admin |
| B2.2 | Privilege escalation via project membership — adding oneself to a project one can already administer elsewhere | E | Attribute check binds membership changes to the *target* project's team, not the subject's | Admin of a team that shares a project |
| B2.3 | Authorization bypass through a missing check on a new endpoint | E | Deny-by-default at the interceptor: an endpoint without a declared required permission fails closed rather than open | A wrongly-declared permission |
| B2.4 | Confused deputy — API acts on behalf of a subject with its own privileges | E | Subject identity propagated through the job payload; the worker authorizes against the *original* subject, not the worker's identity | — |
| B2.5 | Audit tampering | R | `audit_log` is append-only; no UPDATE or DELETE grant for the application role; ships to external sink | Compromise of the database superuser |
| B2.6 | Idempotency key collision across tenants | T | Key namespaced by project ID and derived server-side, never accepted verbatim from the client | — |

### B3 — Data tier

| ID | Threat | STRIDE | Mitigation | Residual |
|---|---|---|---|---|
| B3.1 | Database dump exfiltration | I | Envelope encryption; KEK outside Postgres ([ADR-0008](adr/0008-envelope-encryption.md)) — dump yields ciphertext only | Dump plus KEK compromise |
| B3.2 | Read-replica or backup misconfiguration | I | Same as B3.1 — encryption at the column level makes the exposure survivable | — |
| B3.3 | SQL injection | T/I | Parameterized queries throughout; no string-built SQL; enforced in review and by lint | A raw query added later |
| B3.4 | Event stream eavesdropping on NATS | I | mTLS between services; secrets never placed in events by construction | Broker compromise reveals metadata |
| B3.5 | Job state tampering (direct DB write) | T | Least-privilege DB roles; state transitions validated in domain code, not trusted from the row | Compromise of DB credentials |

### B4 — Worker

| ID | Threat | STRIDE | Mitigation | Residual |
|---|---|---|---|---|
| B4.1 | Memory scraping of decrypted secrets | I | Decrypt immediately before use, narrowest possible lifetime, no swap on worker hosts, core dumps disabled | Root on the worker host reads process memory |
| B4.2 | Secrets leaking into deploy logs | I | Log lines scrubbed against known secret values before emission; secrets never in the event stream | **Transformed secrets defeat scrubbing** — see §5 |
| B4.3 | Two workers deploying the same job | T | Lease with version check; `SKIP LOCKED`; provider idempotency key | Provider that ignores the key |
| B4.4 | Malicious job payload from a compromised broker | T | Job payload is a reference (deployment ID), not the instruction set; the worker re-reads authoritative state from Postgres | — |
| B4.5 | Worker retains credentials after job completion | I | Explicit zeroing and scope exit; no credential caching between jobs | Go's GC gives no guarantee of prompt erasure |

### B5 — Build container (untrusted code)

| ID | Threat | STRIDE | Mitigation | Residual |
|---|---|---|---|---|
| B5.1 | Malicious build script exfiltrates secrets | I | **No network egress by default**; build env contains only declared build-time variables; deploy credentials never enter the container | An allow-listed egress destination |
| B5.2 | Build script reads worker credentials | I | Container has no access to the worker's environment, filesystem, or instance metadata endpoint (169.254.169.254 blocked) | Container escape |
| B5.3 | Container escape to worker host | E | Non-root user, dropped capabilities, seccomp profile, read-only root filesystem, no privileged mode, no Docker socket mount | Kernel vulnerability |
| B5.4 | Resource exhaustion (fork bomb, disk fill, CPU) | D | CPU, memory, PID, and disk quotas; hard build timeout (`VEYRONIX_BUILD_TIMEOUT`) | Noisy-neighbour latency impact |
| B5.5 | Poisoned dependency in the user's own supply chain | T | Out of scope for Veyronix to prevent; contained by B5.1 and B5.2 so it cannot reach the platform | Compromised artifact deploys — user's supply chain |
| B5.6 | Build artifact tampering between build and deploy | T | Artifact digest recorded at build, verified before `Provider.Deploy` | — |

### B6 — Provider layer

| ID | Threat | STRIDE | Mitigation | Residual |
|---|---|---|---|---|
| B6.1 | Provider token exfiltration by a malicious third-party provider | I | Providers receive only the credentials for their own target; conformance review before registry inclusion; providers run in-process, so this is a **trust decision, not a technical control** | See "Accepted risks" |
| B6.2 | Over-privileged provider credential | E | Documented minimum IAM policy per provider in [`providers/`](providers/); `Validate` warns on detectably excessive scope | User grants admin anyway |
| B6.3 | SSRF via user-supplied provider endpoint | I | Endpoint allow-listing per provider type; link-local and private ranges blocked unless explicitly configured for self-hosted targets | Misconfigured allow-list |
| B6.4 | Provider API compromise upstream | T | Blast radius limited to that provider's targets; circuit breaker and anomaly alerting on provider error rates | Full trust in the provider's own security |
| B6.5 | SSH host key not verified — MITM on VPS deploys | S | Host key pinned at target creation, verified on every connection; a change fails the deploy loudly rather than prompting | Key pinned during an active MITM |

---

## 4. Attack chains worth walking

Individually mitigated threats can still compose. Three chains that were explicitly analysed:

**Chain A — developer to production compromise.**
`Compromised developer laptop → valid session → deploy malicious commit to a project the developer legitimately owns → production compromise.`
This chain is *not fully mitigated by design*, because the developer is authorized to do it. Controls are detective and procedural rather than preventive: approval workflows on production environments, audit records, anomaly alerting on unusual deploy patterns, and short session TTLs. Accepting this is a deliberate choice — a platform that prevents authorized users from deploying is a platform nobody uses.

**Chain B — build container to credential theft.**
`Malicious dependency → build script runs → attempts to reach the worker environment or the network.`
Broken at B5.1 and B5.2: no egress, no worker environment, no metadata endpoint. Requires a container escape (B5.3) to progress, and the escape lands on a host whose credentials are provider tokens for *other* projects — so B4.1's short decryption window matters.

**Chain C — read-only database access to full compromise.**
`SQL injection or leaked replica credential → read secrets table.`
Broken at B3.1: ciphertext only. Requires separately obtaining the KEK, which is not in the database. This is the entire reason for envelope encryption.

---

## 5. Accepted risks

Stated explicitly, because an unlisted accepted risk is an undiscovered one.

1. **In-process providers.** A malicious or buggy provider runs inside the worker with access to that worker's memory. Mitigated by review, not by isolation. Out-of-process plugins are the fix and are deferred ([ADR-0002](adr/0002-provider-interface.md)).
2. **Log scrubbing is best-effort.** A secret that user code transforms — base64, chunked, reversed — will not match the scrubber. The structural mitigation is that deploy credentials never enter the build container at all (B5.1); scrubbing protects against accidents, not adversaries.
3. **KEK loss is unrecoverable.** By design. A KEK escrow procedure that an attacker could use is not a security control. Backup and recovery must be documented and *tested* — see [RUNBOOK](RUNBOOK.md).
4. **Authorized insiders can deploy malicious code.** See Chain A. Detective controls only.
5. **Long-lived provider tokens.** No dynamic or short-lived credentials in V1. Rotation is manual.
6. **Go's garbage collector gives no erasure guarantee.** Zeroing a byte slice does not guarantee no copy remains.

---

## 6. Security invariants

Testable statements that must never be false. Each has a corresponding test.

| # | Invariant | Verified by |
|---|---|---|
| 1 | No plaintext secret is ever written to disk by any process | Integration test asserting on the filesystem after a deploy |
| 2 | No plaintext secret appears in any event, log line, or metric label | Fuzz test: deploy with a known sentinel secret, grep all outputs |
| 3 | Every mutating RPC has a declared required permission | Compile-time check over the handler registry |
| 4 | An audit record exists for every state-changing action, written in the same transaction | Property test over the audit log versus the deployment table |
| 5 | A subject with no matching policy is denied | Authorization test suite, deny-by-default case |
| 6 | The build container cannot reach the network or the metadata endpoint | Container integration test |
| 7 | A ciphertext moved between projects fails to decrypt | Crypto unit test on AAD binding |

---

## 7. Review cadence

Re-examine this document when a new trust boundary appears (out-of-process providers, a public provider registry, multi-tenancy across organizations), when a new provider class is added, and at minimum every six months. Record changes here rather than in commit messages alone.
