# flagpost

flagpost is a self-hosted feature-flag and experimentation service written in Go. It stores flags, targeting rules and segments in SQLite, pushes every change to connected SDKs over server-sent events, and ships a Go SDK that evaluates flags in-process with the same engine the server uses. A small server-rendered admin UI and a REST API sit on top of one service layer, so every change, from any entry point, is validated, audited with a structural diff and streamed the same way.

It is a single static binary with no external dependencies beyond a writable directory for the database.

## Features

- **Flags with typed variants.** Boolean, string, number and JSON flags. Each flag has a list of variants; environments decide which variant to serve.
- **Environments.** Independent configuration per environment (production, staging, ...), each with its own SDK key and a monotonically increasing revision.
- **Targeting engine.** Evaluation walks a fixed order: kill switch, prerequisites, individual targets, rules (first match wins), fallthrough. Clauses support 19 operators: equality and set membership, string prefix/suffix/contains, regular expressions, numeric comparison, time before/after, semantic-version comparison, attribute existence and segment membership. Attributes can be nested (`org.tier`) and array-valued.
- **Percentage rollouts with consistent hashing.** A context's bucket is derived from SHA-256 of the flag salt and a chosen attribute, scaled onto 100,000 buckets with a multiply-shift instead of a biased modulo. Growing a rollout from 10% to 20% only adds contexts; nobody who already had the feature loses it. Rollouts can bucket by any attribute (for example `org` so that a whole company moves together).
- **Segments.** Reusable per-environment groups with explicit include and exclude lists and rule-based membership, optionally weighted. Segments may reference other segments; cycles are rejected at write time and detected at evaluation time.
- **Prerequisites.** A flag can require another flag to serve a specific variant. The service rejects configurations that would create a prerequisite cycle (depth-first search with three-colour marking) and the evaluator independently guards against cycles and excessive depth.
- **Explainable evaluation.** Every result carries a reason (`OFF`, `TARGET_MATCH`, `RULE_MATCH`, `FALLTHROUGH`, `PREREQUISITE_FAILED`, `ERROR`) and the matching rule. The admin API and UI can return a full decision trace.
- **Experiments.** Mark a rollout as an experiment and the SDK reports exposures (deduplicated per context) and `Track` reports conversions. The server attributes only conversions that happen after a context's first exposure, then reports per-variant conversion rates with Wilson score intervals, lift, two-proportion z-tests with a Bonferroni correction for multiple arms, and a chi-square sample ratio mismatch check against the configured weights.
- **Audit log.** Every write records the actor, an optional comment, the before and after documents and a structural diff. Lists of rules and variants are matched by `id`/`key`, so inserting a rule at the top shows up as one added rule rather than every rule "changing". Saves that change nothing are skipped entirely.
- **Optimistic concurrency.** Flags, configurations and segments carry versions. Writes accept the expected version in the body or an `If-Match` header and fail with `412` on a lost update.
- **SSE streaming with resume.** SDKs receive a full snapshot on connect, then small patch and delete events. Each environment keeps a ring buffer of recent events; a client reconnecting with `Last-Event-ID` gets only what it missed, or a fresh snapshot if the buffer has moved on or the server restarted. Slow subscribers are disconnected instead of blocking publishers.
- **SQLite storage with migrations.** Versioned, checksummed migrations embedded in the binary. Editing an applied migration, or running an older binary against a newer schema, is refused. Connections use WAL, foreign keys and `IMMEDIATE` transactions.
- **Go SDK.** Local evaluation, streaming or ETag-based polling, reconnect with full-jitter backoff, an idle watchdog for half-open connections, an optional on-disk snapshot cache for cold starts while the server is down, typed accessors that fall back to defaults, and a bounded event buffer that never blocks the host application.
- **Admin UI.** Server-rendered with `html/template`, no JavaScript. Environments, flags (with on/off toggles and a JSON targeting editor), an evaluation tester with traces, experiment results, segments and the audit log. Signed stateless session cookies and per-session CSRF tokens.

## Architecture

```
                  +-------------------- flagpost binary ---------------------+
 admin UI  -----> |  internal/ui  --+                                         |
 (browser)        |                 |                                         |
 REST API  -----> |  internal/api --+--> internal/service --> internal/store --+--> SQLite (WAL)
 (curl, CI)       |       |         |       |       |            ^            |
                  |       |         |       |       +-> internal/audit (diff) |
                  |       |         |       +-> internal/stream (broker) ---+ |
                  |       |         |       +-> eval (engine, cached        | |
                  |       |         |            per environment revision)  | |
                  +-------+---------+---------------------------------------+-+
                          |                                                 |
                          | /sdk/v1/snapshot, /sdk/v1/events                | /sdk/v1/stream (SSE)
                          v                                                 v
                 +------------------- your service (Go) -----------------------+
                 |  sdk.Client: atomic snapshot -> eval.Evaluator (same engine) |
                 |  event processor: exposures + conversions -> batched POSTs   |
                 +--------------------------------------------------------------+
```

Package overview:

| Package | Responsibility |
| --- | --- |
| `model` | Domain types (flags, configs, segments, snapshots, stream payloads) and validation. Public, shared by server and SDK. |
| `eval` | Pure evaluation engine: bucketing, operators, rules, segments, prerequisites, traces. Public. |
| `sdk` | Go client: SSE parser, streaming and polling transports, event processor, cache file. Public. |
| `internal/store` | SQLite persistence, migrations, optimistic snapshot reads. |
| `internal/service` | Cross-object validation, audit logging, revision bookkeeping, change publication, snapshot cache, experiment analysis. |
| `internal/stream` | Per-environment pub/sub with sequence IDs and a replay ring buffer. |
| `internal/api` | REST and SDK HTTP handlers, auth, error mapping, SSE writer. |
| `internal/ui` | Admin UI handlers, templates and stylesheet. |
| `internal/audit` | Audit entry types and the identity-aware JSON diff. |
| `internal/stats` | Normal quantiles, Wilson intervals, z-test, regularized incomplete gamma, SRM test. |
| `internal/semver` | Semantic Versioning 2.0.0 parser and precedence comparison. |
| `cmd/flagpost` | The server binary and operational subcommands. |

A few design decisions worth calling out:

- **Writes publish after commit.** Each mutating service call runs one transaction that writes the change, its audit entry and the environment revision bump; stream events are collected during the transaction and published only once it commits.
- **Snapshots are read optimistically.** Reading a snapshot inside an `IMMEDIATE` transaction would take SQLite's write lock. Instead the store reads the environment revision, the rows, then the revision again, and retries if it moved. Because every write bumps the revision in the same transaction, equal revisions prove the read was consistent.
- **The SDK and server share one engine.** `eval` has no I/O, so the evaluate endpoints, the UI tester and the SDK cannot disagree.

## Quick start

Requirements: Go 1.26 or newer. No C compiler is needed; the SQLite driver is pure Go.

```sh
make build
FLAGPOST_ADMIN_TOKEN=change-me-to-something-long \
FLAGPOST_BOOTSTRAP_ENVS=production,staging \
./bin/flagpost serve
```

Open http://localhost:8080/ui/ and sign in with the admin token. `FLAGPOST_BOOTSTRAP_ENVS` creates those environments the first time the server starts with an empty database.

Or with Docker:

```sh
docker build -t flagpost .
docker run -p 8080:8080 -v flagpost-data:/data \
  -e FLAGPOST_ADMIN_TOKEN=change-me-to-something-long \
  -e FLAGPOST_BOOTSTRAP_ENVS=production,staging \
  flagpost
```

### Configuration

| Flag | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `-addr` | `FLAGPOST_ADDR`, else `PORT` | `:8080` | Listen address. `PORT` alone gives `:$PORT`. |
| `-db` | `FLAGPOST_DB` | `flagpost.db` | SQLite database path. |
| `-admin-token` | `FLAGPOST_ADMIN_TOKEN` | (required) | Admin API and UI credential, at least 16 characters. |
| `-bootstrap-envs` | `FLAGPOST_BOOTSTRAP_ENVS` | | Environments to create on first start. |
| `-log-format` | `FLAGPOST_LOG_FORMAT` | `text` | `text` or `json`. |
| `-log-level` | `FLAGPOST_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `-heartbeat` | | `25s` | Keep-alive interval on SDK streams. |

Other subcommands: `flagpost migrate` prints applied migrations, `flagpost bucket <seed> <value>...` prints the bucket a value hashes to (handy when someone asks "why am I not in the 10%?"), and `flagpost version`.

## Using the REST API

Admin endpoints take `Authorization: Bearer <admin token>`. An optional `X-Flagpost-Actor` header names the person or system in the audit log.

```sh
T="Authorization: Bearer $FLAGPOST_ADMIN_TOKEN"

# Define a flag (it starts off everywhere).
curl -H "$T" -X POST localhost:8080/api/v1/flags -d '{
  "key": "new-checkout", "name": "New checkout", "kind": "boolean",
  "variants": [{"key": "on", "value": true}, {"key": "off", "value": false}]
}'

# Target it in production: pro users get it, 25% of everyone else too.
curl -H "$T" -X PUT localhost:8080/api/v1/environments/production/flags/new-checkout -d '{
  "version": 0,
  "enabled": true,
  "offVariant": "off",
  "rules": [{
    "id": "pro",
    "clauses": [{"attribute": "plan", "operator": "eq", "values": ["pro"]}],
    "serve": {"variant": "on"}
  }],
  "fallthrough": {"rollout": {"variants": [
    {"variant": "on", "weight": 25000}, {"variant": "off", "weight": 75000}
  ]}},
  "comment": "start the gradual rollout"
}'

# Ask why a user gets what they get.
curl -H "$T" -X POST localhost:8080/api/v1/environments/production/evaluate -d '{
  "flag": "new-checkout", "explain": true,
  "context": {"key": "user-42", "attributes": {"plan": "free"}}
}'
```

Weights are in thousandths of a percent and must sum to 100000.

| Method and path | Purpose |
| --- | --- |
| `GET, POST /api/v1/environments` | List or create environments. |
| `GET, PATCH, DELETE /api/v1/environments/{env}` | Read, rename or delete an environment. |
| `POST /api/v1/environments/{env}/sdk-key` | Rotate the SDK key. |
| `GET /api/v1/environments/{env}/snapshot` | The complete evaluation state SDKs receive. |
| `POST /api/v1/environments/{env}/evaluate` | Evaluate one flag (optionally with a trace) or all flags. |
| `GET, POST /api/v1/flags` | List (`?q=`, `?tag=`, `?archived=true`) or create flags. |
| `GET, PUT, DELETE /api/v1/flags/{flag}` | Read, update or delete a flag definition. |
| `GET /api/v1/environments/{env}/flags` | Every flag with its configuration in the environment. |
| `GET, PUT /api/v1/environments/{env}/flags/{flag}` | Read or replace targeting. |
| `POST /api/v1/environments/{env}/flags/{flag}/toggle` | Turn a flag on or off. |
| `GET /api/v1/environments/{env}/flags/{flag}/results?metric=` | Experiment analysis. |
| `GET, POST /api/v1/environments/{env}/segments` | List or create segments. |
| `GET, PUT, DELETE /api/v1/environments/{env}/segments/{segment}` | Manage one segment. |
| `GET /api/v1/audit` | Audit log, newest first (`env`, `type`, `key`, `actor`, `limit`, `before`). |
| `GET /sdk/v1/snapshot` | SDK snapshot with `ETag`/`If-None-Match`. |
| `GET /sdk/v1/stream` | SSE stream: `put`, `patch`, `delete`. Honours `Last-Event-ID`. |
| `POST /sdk/v1/events` | Exposure and conversion batches. |
| `POST /sdk/v1/evaluate` | Server-side evaluation for clients without a local engine. |
| `GET /healthz` | Liveness and database check. |

SDK endpoints take the environment's SDK key in `Authorization` (or `?sdkKey=` for browser `EventSource`). Errors are JSON: `{"error": {"code": "validation_failed", "message": "...", "problems": [{"field": "rules[0].clauses[1].values[0]", "message": "must be a number"}]}}`.

## Using the Go SDK

```go
client, err := sdk.New(sdk.Config{
    BaseURL:   "http://flagpost:8080",
    SDKKey:    os.Getenv("FLAGPOST_SDK_KEY"),
    CacheFile: "/var/cache/myapp/flags.json", // optional: survive server outages at startup
})
if err != nil {
    log.Fatal(err)
}
defer client.Close() // flushes buffered experiment events

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := client.WaitReady(ctx); err != nil {
    log.Printf("flags not ready, serving defaults: %v", err)
}

user := eval.NewContext(userID).With("plan", "pro").With("country", "NZ")
if client.Bool("new-checkout", user, false) {
    // ...
}
label := client.String("cta-label", user, "Buy")
client.Track("purchase", user) // conversion for experiments

res := client.Evaluate("new-checkout", user) // full result with reason
```

Evaluation never blocks on the network. Before the first snapshot arrives, accessors return the supplied default and `Evaluate` reports `CLIENT_NOT_READY`. A runnable example lives in `examples/quickstart`.

## Running the tests

```sh
go test ./...                # everything
go test -race -count=1 ./... # what CI runs
make lint                    # gofmt, go vet, staticcheck
```

The suite is table-driven throughout and uses no mocks for storage: store, service, API, UI and SDK tests run against real SQLite databases in temporary directories, and the SDK tests drive a real flagpost HTTP server, including a proxy that cuts the stream to exercise reconnection and resume. The bucketing tests pin golden hash values and check uniformity with a chi-square test; the statistics tests check the incomplete gamma implementation against closed forms.

## Benchmarks

Measured with `go test -run '^$' -bench . -benchmem -benchtime 2s -count 5 ./eval/ ./sdk/` on an AMD Ryzen 7 7800X3D under WSL2, Go 1.27.2. The machine was busy with other work during the run (load average around 25), so treat these as rough, and expect lower numbers on an idle machine. Medians of five runs:

| Benchmark | What it does | Median | Allocations |
| --- | --- | --- | --- |
| `Bucket` | SHA-256 bucketing of one value | 87 ns/op | 0 |
| `EvaluateFallthroughRollout` | 11 non-matching rules (one a segment lookup against 10,000 members), then a 50/50 rollout | 1.7 µs/op | 2 (368 B) |
| `EvaluateSegmentMember` | Same flag, context matched by segment membership | 506 ns/op | 1 (352 B) |
| `EvaluateParallel` | The rollout case across 14 goroutines | 185 ns/op | 2 (368 B) |
| `ClientBool` | `sdk.Client.Bool` on a simple enabled flag | 335 ns/op | 2 (353 B) |

## Roadmap

These are known gaps, not promises:

- **Single admin credential.** There is one admin token; no per-user accounts, roles or approval workflows. The audit actor comes from a header the caller sets.
- **Single instance.** The stream broker is in-process, so running several replicas behind a load balancer would leave SDKs connected to one replica unaware of writes made through another. A shared notification channel (or a Postgres `LISTEN/NOTIFY` backend) is the natural next step.
- **SQLite only.** The store is written against SQLite; a Postgres implementation would need its own migrations.
- **Binary conversion metrics only.** Experiments measure "did the context convert after exposure". There are no revenue or count metrics, no sequential testing, and no variance reduction.
- **No event retention policy.** Exposures and conversions are kept until an environment is deleted or an experiment is reset through the store.
- **JSON rule editing in the UI.** Targeting is edited as a JSON document with server-side validation; there is no structured rule builder.
- **Go SDK only.** Other languages can use the snapshot, stream and evaluate endpoints directly, but there are no client libraries for them.
- **No scheduled changes or webhooks.**

## License

MIT. See [LICENSE](LICENSE).
