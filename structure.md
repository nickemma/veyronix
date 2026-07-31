## Top level
veyronix/
├── cmd/
│   ├── veyronix-api/main.go        # control plane HTTP/gRPC
│   ├── veyronix-worker/main.go     # job executor
│   ├── veyronix-relay/main.go      # outbox → NATS
│   ├── veyronix-migrate/main.go
│   └── veyronix/main.go            # CLI
│
├── api/
│   ├── proto/veyronix/v1/          # deployment.proto, project.proto, ...
│   ├── gen/go/                     # buf-generated (gitignored or checked in)
│   └── openapi.yaml
│
├── internal/
│   ├── app/                        # COMPOSITION ROOT — only place adapters meet ports
│   │   ├── container.go
│   │   ├── api.go                  # builds the API process
│   │   ├── worker.go               # builds the worker process
│   │   └── relay.go
│   │
│   ├── platform/                   # shared kernel: technical, zero domain knowledge
│   │   ├── config/                 # env → typed config, validated at boot
│   │   ├── postgres/               # pool, tx manager, migration runner
│   │   ├── outbox/                 # generic outbox writer + relay loop
│   │   ├── messaging/              # NATS JetStream pub/sub abstraction
│   │   ├── crypto/                 # KEK/DEK envelope encryption, log scrubber
│   │   ├── telemetry/              # otel tracer, prom registry, slog
│   │   ├── errors/                 # error taxonomy → Connect codes
│   │   ├── id/                     # dpl_01HQ8-style typed IDs
│   │   └── clock/                  # injectable time (leases need this testable)
│   │
│   ├── modules/
│   │   ├── identity/
│   │   ├── authz/
│   │   ├── project/
│   │   ├── secrets/
│   │   ├── deployment/
│   │   ├── audit/
│   │   └── notification/
│   │
│   └── providers/                  # driven adapters implementing sdk.Provider
│       ├── registry/registry.go
│       ├── netlify/
│       ├── ssh/
│       ├── heroku/
│       ├── docker/
│       └── kubernetes/
│
├── sdk/                            # PUBLIC — third parties import this
│   ├── provider.go                 # the Provider interface + Target/Release/Capabilities
│   ├── conformance/                # suite any provider must pass
│   └── testing/                    # fakes, recorders
│
├── migrations/                     # 0001_projects.up.sql, ...
├── web/                            # Next.js dashboard
├── deploy/                         # docker-compose, k8s manifests, grafana dashboards
├── docs/
└── Makefile


#### Anatomy of a module

Every module under internal/modules/ has the same five folders. Using deployment, the one with real weight:

deployment/
├── domain/                     # pure Go, no imports outward, no db/proto tags
│   ├── deployment.go           # aggregate: Deployment
│   ├── state.go                # State enum + Transition(to) error — the state machine
│   ├── job.go                  # Job, Lease, heartbeat semantics
│   ├── idempotency.go          # key derivation from (project, env, sha)
│   ├── event.go                # DeploymentEvent value objects
│   └── errors.go               # ErrInvalidTransition, ErrLeaseLost, ...
│
├── ports/                      # interfaces this module needs or offers
│   ├── inbound.go              # DeploymentService: Create, Cancel, Rollback, Stream
│   ├── repository.go           # JobRepository, DeploymentRepository, LeaseStore
│   ├── publisher.go            # EventPublisher
│   ├── provider.go             # ProviderRegistry (returns sdk.Provider by name)
│   └── external.go             # SecretResolver, Authorizer, AuditRecorder, Notifier
│                               #   ← other modules' use cases satisfy these
│
├── app/                        # use cases — orchestration only, no SQL, no HTTP
│   ├── create_deployment.go    # validate → authz → idempotency → tx{job + outbox}
│   ├── claim_job.go            # lease acquisition + heartbeat loop
│   ├── execute_deployment.go   # clone → build → Deploy → HealthCheck → rollback?
│   ├── cancel.go
│   ├── rollback.go
│   └── stream_events.go
│
├── adapters/
│   ├── postgres/               # driven: repository impls, sqlc or raw pgx
│   │   ├── job_repository.go
│   │   ├── deployment_repository.go
│   │   ├── lease_store.go      # SELECT ... FOR UPDATE SKIP LOCKED + version check
│   │   └── queries/
│   ├── nats/
│   │   └── event_publisher.go
│   ├── connect/                # driving: RPC handlers → app use cases
│   │   ├── handler.go
│   │   └── mapper.go           # proto ↔ domain. Domain never sees proto.
│   └── worker/                 # driving: queue consumer → app.ExecuteDeployment
│       └── consumer.go
│
└── module.go                   # New(Deps) (*Module, error) — module-local wiring


Where the provider interface lives

Put Provider in sdk/, not in internal/. That single decision is what makes the "third parties can write providers" claim in your README true instead of aspirational — external code can't import internal/.

internal/modules/deployment/ports/provider.go then holds only:

package ports

import "github.com/nickemma/veyronix/sdk"

type ProviderRegistry interface {
	Get(name string) (sdk.Provider, error)
	List() []sdk.Descriptor
}

The engine's entire knowledge of Netlify is that string.

#### Composition root
// internal/app/worker.go
func BuildWorker(ctx context.Context, cfg config.Config) (*Worker, error) {
	tel, err := telemetry.New(ctx, cfg.OTel)
	db, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	bus, err := messaging.NewJetStream(cfg.NatsURL)
	kms := crypto.NewEnvelope(cfg.KEK)

	// modules, inner first
	audit := auditmod.New(auditmod.Deps{DB: db})
	secrets := secretsmod.New(secretsmod.Deps{DB: db, Sealer: kms, Audit: audit.Recorder()})

	registry := providerreg.New(
		netlify.New(cfg.HTTP),
		sshprov.New(),
		heroku.New(cfg.HTTP),
	)

	deploy := deploymentmod.New(deploymentmod.Deps{
		Jobs:      pgadapter.NewJobRepository(db),
		Leases:    pgadapter.NewLeaseStore(db, clock.System),
		Events:    natsadapter.NewPublisher(bus),
		Providers: registry,
		Secrets:   secrets.Resolver(),   // ports.SecretResolver, satisfied by another module
		Audit:     audit.Recorder(),
		Clock:     clock.System,
	})

	return NewWorker(deploy.Consumer(), cfg.WorkerConcurrency, tel), nil
}

Every dependency is an interface from a ports package. Swapping Postgres for something else, or NATS for Kafka, touches this file and one adapter folder.
