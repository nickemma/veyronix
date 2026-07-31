# Veyronix Provider SDK

**Write your own deployment target.**

```
go get github.com/nickemma/veyronix/sdk
```

---

## What this package is for

The Veyronix deployment engine does not know what a provider is. It knows only that something implements the interface below. Netlify is roughly 200 lines behind it. So is SSH. So is Kubernetes.

That means adding a new deployment target — a cloud you use, an internal orchestrator, a model server — is writing an implementation of one interface and registering it. The engine is untouched. The dashboard is untouched. The permission model is untouched.

This package is deliberately **outside** `internal/`, so code that is not Veyronix can import it. That is the whole point: a provider system that only its author can extend is not a provider system.

> **Status:** the interface is drafted and the engine is a skeleton. It will move before V1. Pin a commit.

---

## The interface

```go
package sdk

// Provider is the only thing the deployment engine knows about a target.
type Provider interface {
	// Name returns the registry identifier, e.g. "netlify", "k8s".
	Name() string

	// Validate checks target configuration and credentials before any
	// mutating call. Runs at project-save time, not at deploy time.
	Validate(ctx context.Context, target Target) error

	// Deploy performs the release. It must be idempotent with respect to
	// req.IdempotencyKey: a repeated call with the same key must not
	// produce a second release.
	Deploy(ctx context.Context, req DeployRequest) (Release, error)

	// Rollback restores a previously successful release.
	Rollback(ctx context.Context, to Release) error

	// Status reports the live state of a release at the target.
	Status(ctx context.Context, rel Release) (ReleaseStatus, error)

	// Logs streams provider-side logs until ctx is cancelled.
	Logs(ctx context.Context, rel Release, opts LogOptions) (<-chan LogLine, error)

	// HealthCheck verifies the release is serving. Returning an error
	// triggers rollback if the environment has it enabled.
	HealthCheck(ctx context.Context, rel Release) error

	// Capabilities declares what this provider supports so the engine and
	// UI can degrade gracefully.
	Capabilities() Capabilities
}
```

---

## The five rules

These are contract obligations, not style advice. The conformance suite tests each one.

### 1. `Deploy` must be idempotent

The queue delivers at-least-once. Your `Deploy` may be called twice with the same `IdempotencyKey`, and the second call must not produce a second release.

If the target has native idempotency, pass the key through. If it does not — SSH, for instance — record the key at the target and check it first:

```go
func (p *SSHProvider) Deploy(ctx context.Context, req sdk.DeployRequest) (sdk.Release, error) {
	if rel, found, err := p.ledger.Lookup(ctx, req.IdempotencyKey); err != nil {
		return sdk.Release{}, err
	} else if found {
		return rel, nil // already done; return the same release
	}
	// ... perform the deploy, then record the key with the release
}
```

This is the single most important obligation in the SDK. Getting it wrong produces duplicate production releases, which is the failure mode the entire durable-job design exists to prevent.

### 2. `Validate` runs early, and does real work

`Validate` is called when a human saves the target configuration, not when a deploy runs. Check that credentials authenticate, that the target exists and is reachable, and that required config is present and coherent.

Every check you do here is an error surfaced to someone looking at a form, rather than discovered at 02:00 with a half-applied release.

### 3. `Capabilities()` must be honest

```go
func (p *NetlifyProvider) Capabilities() sdk.Capabilities {
	return sdk.Capabilities{
		Rollback:     true,
		LogStreaming: true,
		ZeroDowntime: true,
		BlueGreen:    false,
		Canary:       false, // no traffic-splitting primitive. Say so.
	}
}
```

Declaring a capability you do not truly have is worse than declaring none. The platform hides controls for unsupported capabilities and rejects unsupported strategies at `Validate` time — a "canary" that is silently a full replacement is exactly the leaky abstraction this design exists to avoid.

### 4. Classify your errors

Error class determines whether a failure counts against the platform's SLO. A user's failing build is not a platform failure, and misclassifying it corrupts the numbers everyone relies on.

```go
return sdk.Release{}, sdk.Errorf(sdk.ClassUserBuildFailed, "build exited %d", code)
```

| Class | Use when | Counts against platform SLO |
|---|---|---|
| `ClassUserBuildFailed` | Their code did not build | No |
| `ClassUserHealthcheckFailed` | Their app is not healthy | No |
| `ClassProviderError` | Target API returned an error | No (tracked separately) |
| `ClassProviderRateLimited` | 429 — engine backs off and requeues | No |
| `ClassTargetNotFound` | Site, app, or host is gone | No |
| `ClassReleaseNotFound` | Rollback target no longer exists | No — but it **pages** |
| `ClassPlatformInternal` | Your provider has a bug | Yes |

### 5. Respect the context

Cancellation is a real user action — someone clicked Cancel — and it propagates from the browser through the engine to your provider call. `Logs` must close its channel when the context is cancelled. Long-running calls must abort. A provider that ignores cancellation makes a stuck deploy unstoppable.

---

## Writing one

```go
package myprovider

import (
	"context"
	"github.com/nickemma/veyronix/sdk"
)

type Provider struct {
	client *http.Client
}

func New(c *http.Client) *Provider { return &Provider{client: c} }

func (p *Provider) Name() string { return "myprovider" }

func (p *Provider) Validate(ctx context.Context, t sdk.Target) error {
	cfg, err := sdk.DecodeConfig[Config](t.Config)
	if err != nil {
		return sdk.Errorf(sdk.ClassInvalidConfig, "decode config: %w", err)
	}
	return p.ping(ctx, cfg, t.Credentials)
}

func (p *Provider) Deploy(ctx context.Context, req sdk.DeployRequest) (sdk.Release, error) {
	// 1. check the idempotency key
	// 2. upload or reference the artifact
	// 3. cut the release
	// 4. return a Release the engine can later roll back to
}

// ... Rollback, Status, Logs, HealthCheck, Capabilities
```

Then register it:

```go
registry := providerreg.New(
	netlify.New(httpClient),
	myprovider.New(httpClient),
)
```

That is the entire integration. There is no engine change, no migration, no dashboard work.

---

## Conformance suite

```go
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Config{
		Provider: myprovider.New(testClient),
		Target:   testTarget,   // against a sandbox account or a recorded fixture
		Skip:     []string{},   // justify anything you skip, in a comment
	})
}
```

The suite covers:

| Test | What it proves |
|---|---|
| `TestDeployIdempotent` | Two `Deploy` calls with one key produce one release |
| `TestDeployConcurrent` | Two simultaneous calls with one key produce one release |
| `TestValidateRejectsBadCredentials` | Failures surface at save time, not deploy time |
| `TestRollbackRestores` | `Status` after rollback reports the previous release |
| `TestRollbackMissingRelease` | Returns `ClassReleaseNotFound`, does not panic |
| `TestLogsTerminate` | Channel closes on context cancel |
| `TestCancelAborts` | An in-flight `Deploy` stops on cancel |
| `TestErrorClassification` | Build failures classify as user error |
| `TestCapabilitiesHonest` | Every declared capability is exercised and works |

**A provider that passes conformance integrates without engine changes.** That is the contract, and it is what keeps the abstraction from rotting as the seventh provider lands.

---

## Test helpers

```go
import "github.com/nickemma/veyronix/sdk/testing"

fake := sdktesting.NewFakeProvider()         // full lifecycle, no infrastructure
fake.FailOn(sdktesting.PhaseHealthCheck)     // force a rollback path
fake.Delay(sdktesting.PhaseDeploy, 2*time.Second)

rec := sdktesting.NewRecorder(realProvider)  // record real calls, replay in CI
```

`FakeProvider` is what lets the engine's own pipeline be tested end to end with no Postgres, no NATS, and no cloud account.

---

## Security obligations

You are handling credentials for someone else's production infrastructure.

- **Never log credentials**, not even at debug level, not even redacted-by-your-own-hand. The engine scrubs known secret values, but that is a safety net, not permission.
- **Never write credentials to disk.** They arrive decrypted in memory and must stay there.
- **Never pass credentials as command-line arguments** — the process table is readable.
- **Verify TLS.** No `InsecureSkipVerify`. For SSH, pin the host key and fail on mismatch rather than prompting.
- **Document the minimum credential scope** your provider needs, in `docs/providers/<name>.md`. If the target's permission model is too coarse to express least privilege, say so plainly rather than implying otherwise.

See [`docs/THREAT_MODEL.md`](../docs/THREAT_MODEL.md) for the full analysis, particularly boundary B6.

Note that providers currently run **in-process** with the worker. A panicking or leaking provider affects that worker. The engine recovers panics at the call boundary and applies timeouts, but isolation is a trust decision rather than a technical control — see [ADR-0002](../docs/adr/0002-provider-interface.md).

---

## Getting a provider merged

1. Implement the interface.
2. Pass `sdk/conformance`.
3. Write `docs/providers/<name>.md` following the structure of the Netlify document — capabilities, config, minimum credential scope, operation mapping, failure modes, manual intervention.
4. Open a PR.

Step 3 is not paperwork. A provider nobody can operate at 02:00 is a provider nobody should depend on.
