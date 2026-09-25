# TBG / CMS Core Transaction Banking Engine

Pure Go reference implementation of a corporate cash-management payout engine:
Redis lock-free lien orchestration, PostgreSQL double-entry ledger with a
zero-sum database trigger, ISO 20022 (`pacs.008`/`pacs.002`) messaging, and a
midnight EOD suspense zero-out reconciler — plus a Postman/Newman contract
test suite and Splunk observability configs.

This is a learning/reference implementation, not a certified banking core.
`Finacle` and `TCS BaNCS` are proprietary, licensed-only platforms; nothing
here connects to them. The value here is the *pattern* — double-entry
invariants, idempotency, lock-free concurrency, ISO 20022 messaging — built
with fully open-source tooling.

## 0. Prerequisites (all free)

| Tool | Install |
|---|---|
| Docker Desktop | https://www.docker.com/products/docker-desktop/ |
| Go 1.22+ | https://go.dev/dl/ |
| Node.js 18+ (optional, for a dashboard) | https://nodejs.org/ |

No cloud account, no paid license, and no Finacle/BaNCS access is required
for any of this.

## 1. Start dependencies

```bash
cd tbg-engine
docker compose up -d
docker compose ps   # wait until postgres and redis show "healthy"
```

This starts PostgreSQL 16 (with `init.sql` auto-applied), Redis Stack, and
Kafka. The ledger schema, zero-sum trigger, and seed accounts
(`00040310001928`, `AC_CMS_SUSPENSE_CLEARING_9999`, `AC_RBI_NOSTRO_0001`) are
created automatically on first boot.

## 2. Fetch Go dependencies

This sandbox cannot reach the Go module proxy, so `go.sum` is not
pre-generated here. On your own machine, with normal internet access:

```bash
go mod tidy
```

This resolves and locks `github.com/lib/pq`, `github.com/redis/go-redis/v9`,
and `github.com/google/uuid` as declared in `go.mod`.

## 3. Seed the corporate float (demo liquidity)

The Redis shadow ledger starts empty. Seed an available balance for the demo
corporate account before firing payouts:

```bash
docker exec -it tbg_redis redis-cli SET corp:avail:00040310001928 50000000.00
```

## 4. Run the engine

```bash
go run ./cmd/server
```

Expected output:
```
TBG/CMS banking engine listening on :8080
```

## 5. Test it

**Success payout:**
```bash
curl -i -X POST http://localhost:8080/api/v1/cms/payout \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: IDEMP-TEST-001" \
  -d '{
    "corporate_account": "00040310001928",
    "amount": 2500000.00,
    "currency": "INR",
    "payment_rail": "RTGS",
    "beneficiary_name": "Larsen and Toubro Limited",
    "beneficiary_acct": "98127391827",
    "beneficiary_ifsc": "SBIN0000123",
    "reference_id": "INV-SEP-001"
  }'
```
Expect `HTTP 200` with `"status":"SETTLED"`, a generated `utr`, and a 64-char
`merkle_hash`.

**Replay the same Idempotency-Key (expect 409):**
```bash
curl -i -X POST http://localhost:8080/api/v1/cms/payout \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: IDEMP-TEST-001" \
  -d '{"corporate_account": "00040310001928", "amount": 2500000.00}'
```

**Request more than available liquidity (expect 422):**
```bash
curl -i -X POST http://localhost:8080/api/v1/cms/payout \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: IDEMP-TEST-999" \
  -d '{"corporate_account": "00040310001928", "amount": 999999999.00, "reference_id": "INV-FAIL"}'
```

**Check recent postings / suspense balance:**
```bash
curl http://localhost:8080/api/v1/cms/stats
```

**Health check:**
```bash
curl http://localhost:8080/healthz
```

### Go integration tests

With the server running:
```bash
go test -tags=integration ./tests/...
```

### Postman / Newman contract tests

```bash
npm install -g newman
newman run tests/postman_collection.json -e tests/postman_environment.json
```
This validates: 200 + balanced double-entry on a fresh payout, 409 on
Idempotency-Key replay, 422 on liquidity exhaustion, and the `/stats`
endpoint shape. Import both JSON files directly into the Postman app to run
or edit them visually instead.

## 6. EOD reconciliation

`internal/service/eod_reconciler.go` exposes `RunEODBatch(ctx, idempotencyKey)`.
It is wired into `main.go` but not scheduled — call it manually for now, or
add a cron trigger (e.g. `github.com/robfig/cron`) that invokes it once per
business day at cut-off. It sums the `AC_CMS_SUSPENSE_CLEARING_9999` balance
and posts a single balancing voucher against `AC_RBI_NOSTRO_0001`, then
verifies the suspense account returns to exactly `0.0000`.

## 7. Splunk observability

Every request emits a single-line structured JSON log to stdout (see
`internal/observability/logger.go`). To wire it into Splunk:

1. Copy `splunk/props.conf` and `splunk/savedsearches.conf` into
   `$SPLUNK_HOME/etc/apps/tbg_engine/local/`.
2. Point a Splunk forwarder or `inputs.conf` monitor at wherever you redirect
   the engine's stdout (e.g. `go run ./cmd/server >> /var/log/tbg-engine/engine.log 2>&1`),
   with `sourcetype = tbg_core_json` and `index = tbg_core`.
3. The saved searches give you: end-to-end transaction trace by
   `correlation_id`, a 5-minute failure-rate alert, an unbalanced-ledger
   reconciliation check, a p99 latency SLA alert, and a daily EOD
   zero-out audit.

No Splunk license? `splunk/props.conf` and the SPL in `savedsearches.conf`
work unmodified against **Splunk Free** (500 MB/day) for local testing.

## 8. Production deployment

**Docker (scratch, <20MB image):**
```bash
docker build -t tbg-engine:latest .
docker run -p 8080:8080 \
  -e POSTGRES_DSN="postgres://user:pass@your-db-host:5432/cms_ledger?sslmode=require" \
  -e REDIS_ADDR="your-redis-host:6379" \
  -e HMAC_SALT="a-real-secret-rotated-regularly" \
  tbg-engine:latest
```

**Free cloud alternatives to paid enterprise infra**, if you want to run this
somewhere other than your own machine:

| Component | Free-tier option |
|---|---|
| PostgreSQL | https://supabase.com or https://neon.tech |
| Redis | https://upstash.com (serverless, free tier) |
| Go engine hosting | https://render.com (free web service, builds from a Dockerfile) |
| Dashboard (if you build one) | https://vercel.com |

Render, Supabase, and Upstash all deploy straight from a GitHub repo with no
credit card required on their free tiers — push this folder to a repo and
connect it there when you're ready.

## 9. What's intentionally out of scope

- No real mTLS/HSM — `X-Signature` here is an HMAC-SHA512 placeholder for the
  *pattern* of non-repudiation, not a production PKI implementation.
- No live NPCI/RBI SFMS connection — `dispatchToClearingRail()` in
  `internal/service/payout.go` is a stub; a real deployment would only ever
  get rail access through your employer's licensed banking infrastructure.
- Kafka is provisioned in `docker-compose.yml` for the topology described in
  the architecture discussion, but the reference HTTP handler processes
  payouts synchronously for clarity. Swap in a Kafka producer/consumer pair
  if you want to decouple ingestion from settlement.
