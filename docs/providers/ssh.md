# Provider — VPS / SSH

| | |
|---|---|
| **Registry name** | `ssh` |
| **Status** | Planned — V1 |
| **Target type** | A machine you can SSH into |
| **Transport** | SSH, key-based authentication only |

---

## Why this provider matters most

Netlify is the easiest provider. This is the hardest, and it is the one that proves the abstraction. A bare VPS has no release concept, no rollback primitive, no deployment API, and no idempotency — Veyronix has to construct all four out of a filesystem and a process supervisor.

If the [`sdk.Provider`](../../sdk/README.md) interface survives this target without leaking, it will survive most others.

---

## Capabilities

```go
sdk.Capabilities{
    Rollback:        true,   // atomic symlink swap
    LogStreaming:    true,   // journalctl / file tail over the session
    ZeroDowntime:    true,   // only with a supervisor that supports reload
    BlueGreen:       false,
    Canary:          false,
    HealthCheck:     true,
    EnvironmentVars: true,
    Cancel:          true,   // context cancel terminates the remote command
}
```

`ZeroDowntime` is conditional on the target's configuration, so it is determined by `Validate` at project-save time and reported per target rather than declared statically for the provider.

---

## Release layout

The Capistrano-style layout, because it makes rollback a single atomic operation:

```
/srv/<app>/
├── releases/
│   ├── 20260726T101500Z-a1b2c3d/
│   ├── 20260727T093000Z-e4f5g6h/
│   └── 20260728T140000Z-i7j8k9l/    ← new release
├── shared/
│   ├── .env                          ← rendered secrets, mode 0600
│   ├── logs/
│   └── uploads/
├── current -> releases/20260728T140000Z-i7j8k9l
└── .veyronix/
    ├── deploys.jsonl                 ← idempotency + release ledger
    └── lock
```

Deploy: upload to a new timestamped directory, link `shared` paths, run the release command, then atomically swap `current` with `ln -sfn` + `mv`. Rollback: swap `current` back and restart. Both are effectively instantaneous, and the swap is atomic on POSIX — there is no window where `current` points at nothing.

Old releases are pruned to the last N (default 5), which is what makes rollback depth a configuration decision rather than an accident.

---

## Configuration

```yaml
provider: ssh
config:
  host: deploy.example.com
  port: 22
  user: deploy
  app_dir: /srv/myapp
  release_command: ./bin/migrate && systemctl --user restart myapp
  health_url: http://127.0.0.1:8080/healthz
  keep_releases: 5
  supervisor: systemd            # systemd | supervisor | none
  service_name: myapp
credentials:
  private_key: <secret-ref>      # required, no passphrase prompt possible
  known_host_key: <pinned>       # captured and pinned at target creation
```

---

## Minimum privilege on the target

The deploy user should be able to deploy the application and nothing more. Concretely:

- A dedicated `deploy` user, **not** root.
- Ownership of `/srv/<app>` only.
- **No general sudo.** If a service restart requires elevation, grant exactly that one command:
  ```
  deploy ALL=(root) NOPASSWD: /bin/systemctl restart myapp, /bin/systemctl status myapp
  ```
  Better still, run the service as a systemd **user** unit so no elevation is needed at all.
- `authorized_keys` entry restricted where possible:
  ```
  restrict,pty,no-agent-forwarding,no-port-forwarding ssh-ed25519 AAAA... veyronix
  ```
- The key is Veyronix-specific and used for nothing else, so revocation is a single line.

---

## Host key pinning

The host key is captured at target creation and pinned. Every subsequent connection verifies against the pinned key, and a mismatch **fails the deployment loudly** — it never prompts and never auto-accepts.

This is deliberate. `StrictHostKeyChecking=no` in a deployment tool is an open invitation to a machine-in-the-middle attack against a session that is about to hand over credentials and push code to production (threat B6.5). A legitimate host key change — a rebuilt server — requires an explicit re-pin by a human:

```bash
veyronix target rekey --project "$PROJECT" --target "$TARGET"
```

---

## Idempotency

SSH has no native idempotency, so the provider builds it: `.veyronix/deploys.jsonl` on the target records each deploy's idempotency key, release directory, and outcome. `Deploy` checks the ledger first — a key already present with a successful outcome returns that release instead of creating a new one.

A lock file (`.veyronix/lock`, acquired with `flock`) prevents two concurrent deploys to the same target even if the platform's own per-environment serialization is bypassed by a manual run.

---

## Secrets handling

Secrets are rendered to `shared/.env` with mode `0600`, owned by the deploy user, written via a temporary file and an atomic rename so a partially-written file is never read by a starting process. They are **never** passed as command-line arguments — the process table is world-readable — and never echoed into the deploy log.

---

## Failure modes

| Failure | Signal | Behaviour |
|---|---|---|
| Host unreachable | Dial timeout | Retry with backoff, then fail; no partial state, nothing was uploaded |
| Host key mismatch | Verification failure | **Fail immediately, do not connect**; requires human re-pin |
| Auth failure | SSH auth error | Fail pre-flight; classified as configuration error |
| Disk full | Upload or extract error | Fail before the symlink swap — the previous release is still live and serving |
| Release command fails | Non-zero exit | Fail before the swap; previous release still live |
| Restart fails after swap | Health check fails | Auto-rollback: swap `current` back, restart, verify |
| Connection drops mid-deploy | Session ends | Job resumes on another worker; ledger prevents duplicate release |
| Rollback fails (previous release pruned) | `release_not_found` | **Pages** — see [runbook](../RUNBOOK.md#rollback-failure); raise `keep_releases` |

The ordering above is the important part: everything that can fail is arranged to fail *before* the symlink swap, so the previous release keeps serving. The swap is the single moment of commitment.

---

## Manual intervention

```bash
ssh deploy@host
cd /srv/myapp
ls -la current releases/            # what is live, what is available
ln -sfn releases/<known-good> current.tmp && mv -Tf current.tmp current
sudo systemctl restart myapp
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz
tail -50 .veyronix/deploys.jsonl    # what the platform thinks happened
```

Record the action in the incident timeline; the platform's record of the live release is now stale.
