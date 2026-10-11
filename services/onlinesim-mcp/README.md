# OnlineSim MCP

Standalone Go service for short-lived OnlineSim SMS activations, initially using India (`country=91`). It exposes five MCP tools over stdio or Streamable HTTP. The default provider is an offline simulator; an API Key is not needed for building or testing.

The service provides phone numbers and OTPs. Website registration, CAPTCHA handling, account roles and authenticated browser/API sessions remain separate ARTEX workflows.

## Build and test

Requires Go 1.26.3. Durable state locking currently supports Linux and macOS. This is an independent module: root-level `go test ./...` does not run its tests.

```sh
cd services/onlinesim-mcp
go test -race ./...
go vet ./...
go build -o onlinesim-mcp ./cmd/onlinesim-mcp
```

Dependencies are pinned in `go.mod` and `go.sum`. The official MCP Go SDK handles protocol negotiation, input schemas, stdio framing and Streamable HTTP. ARTEX's `2025-06-18` protocol version is covered by the HTTP tests.

## Offline stdio demo

```sh
./onlinesim-mcp -mode mock -transport stdio -task-id local-test
```

stdout is exclusively MCP JSON-RPC. Diagnostics go to stderr. Connect with a MCP client and call:

1. `sms.quote` with `{}` to discover the simulated service catalog.
2. `sms.reserve` with `{"service":"demo","idempotency_key":"registration-1","max_price":"0.20"}`.
3. `sms.status` with the returned `order_id`; the background poll retrieves a synthetic code.
4. `sms.next_message` to advance to a second synthetic message, if needed.
5. `sms.close`, then `sms.status` until `closed`.

Every result reports `mode: mock`. The mock phone number is intentionally non-operational. Never use these fixtures against a real website. Mock fixture services:

| Service | Behavior |
| --- | --- |
| `demo` | SMS available after two seconds; a new pointer yields a different code |
| `no_stock` | Zero stock, rejected before purchasing |
| `late_sms` | No SMS; the wait deadline triggers cleanup |
| `close_guard` | Closure blocked for 120 seconds; cleanup retries with backoff |
| `purchase_timeout` | Simulated purchase succeeds upstream but its response is lost; no automatic repurchase |

stdio has one trusted task identity and an exclusive state directory. Use HTTP for ARTEX's concurrent workers; multiple stdio processes cannot share one directory.

## HTTP and ARTEX

Configure a separate service token in `ONLINESIM_MCP_TOKEN` (at least 16 characters, provision it through your local secret configuration). This is not the OnlineSim API Key.

```sh
./onlinesim-mcp -mode mock -transport http -listen 127.0.0.1:8790
```

- MCP endpoint: `http://127.0.0.1:8790/mcp`
- Health endpoint: `http://127.0.0.1:8790/healthz`
- All MCP requests require `Authorization: Bearer <service-token>` and `X-ARTEX-Task-ID: <trusted-task-id>`.
- Bind to loopback by default. If exposed through a gateway, terminate HTTPS and restrict access to the trusted ARTEX backend. Browser-origin requests are rejected.

In ARTEX's MCP settings, add a server named `onlinesim`, transport `http`, the reachable MCP URL, and the following request headers:

```json
{
  "Authorization": "Bearer <service-token>",
  "X-ARTEX-Task-ID": "{{task_id}}"
}
```

`examples/artex-http.json` contains a disabled configuration template. It contains no real credentials. The service URL must be reachable from ARTEX's runtime: `127.0.0.1` inside a container refers to that container, not to the host.

This branch adds opt-in `{{task_id}}` expansion to ARTEX's MCP connector in only `X-ARTEX-Task-ID` and `ARTEX_TASK_ID`. The backend injects the active task registry ID; chat runs use `chat:<session-id>` and tool discovery uses `discovery`. Discovery can list/quote tools but cannot purchase numbers. Other environment/header values remain literal.

Grant MCP visibility to the intended Agent in ARTEX. Identity is derived from the authenticated HTTP request or trusted stdio launcher, never from an LLM-supplied `task_id` argument. The HTTP service token authorizes one trusted backend to assert task identities; it is not a separate credential for each tenant or task. Keep it outside prompts and tool arguments.

## Tools

| Tool | Inputs | OnlineSim method |
| --- | --- | --- |
| `sms.quote` | optional `service`, `page` | `getTariffs` |
| `sms.reserve` | `service`, stable `idempotency_key`, `max_price`, optional `wait_seconds` | `getNum` |
| `sms.status` | `order_id` | locally cached `getState` result |
| `sms.next_message` | `order_id` | `setOperationRevise` |
| `sms.close` | `order_id` | asynchronous `setOperationOk` |

`quote` returns service slugs, available stock, tariff strings and pagination. Always select a supported service; India availability does not imply support for any arbitrary website.

`reserve` returns a durable broker reference, number when available, state, deadline and tariff estimate. Reusing the key in the same task returns the original order, including a closed/failed/unknown result. A new attempt needs a new key and consumes the task's attempt allowance. Default limits are two purchases per task, a maximum preflight tariff of `1.00`, gross estimated budget `5.00`, a ten-minute wait deadline and five-second polling. CLI flags can change these limits.

Prices are decimals in the provider profile's tariff units. `-price-unit` labels the configured unit; it defaults to `profile_tariff_units`, not an assumed currency. The OnlineSim `getNum` documentation has no upstream maximum-price argument. Price and budget controls therefore apply to the **preflight tariff estimate**, not a guaranteed final charge. Gross estimates remain reserved after closure; no refund is assumed. The `getState.sum` field is not combined with tariff prices because its currency/unit has not been verified. Validate actual charges after configuring the real provider.

`status` reads a task-scoped cache and does not block waiting for SMS. The service polls in the background, shortens the deadline if the provider reports an earlier expiry and uses bounded backoff on upstream errors. OTPs are retained only in memory and are excluded from `state.json`. The status tool returns OTPs to its caller: a host that records all tool outputs or LLM prompts must redact these fields before real usage. This service does not modify ARTEX's conversation/traffic recording policy.

`next_message` advances OnlineSim's message pointer; it does not trigger a new SMS at the website. Pointer changes are never automatically retried on ambiguous outcomes. A pending pointer operation stays blocked after interruption; inspect the provider state rather than calling it repeatedly.

`close` persists the cleanup request and returns `close_pending`. Protective intervals (typically 120 seconds, 300 seconds for WhatsApp) and rate limits are respected with backoff. The service needs to remain running to finish cleanup. Shutdown preserves pending orders; startup resumes polling and cleanup. Unknown purchases cannot be closed until their provider operation ID is reconciled.

## Durable state and recovery

Default state directories are `./data/mock` and `./data/live`. Use `-data-dir` for a persistent deployment volume. The directory is private (`0700`), snapshots and lock files use `0600`, snapshots use fsync plus atomic rename, and an OS lock prevents multiple writers. Mock and live state cannot be mixed. Live state is bound to a non-secret fingerprint of the API Key and endpoint, and to the price-unit label; account/unit changes require a separate directory. State write failure stops further mutations.

A purchase intent is saved **before** calling OnlineSim. If a transport error, malformed response or process crash leaves the outcome unknown, the order becomes `purchase_unknown`; retries with the same key do not purchase again. Further purchases for that task are blocked. A dedicated OnlineSim account is recommended to make operator reconciliation unambiguous.

After verifying the exact operation in the OnlineSim dashboard, stop the owning service and run the operator-only recovery command with the same mode, state directory and account configuration:

```sh
./onlinesim-mcp -mode live -allow-live-orders -task-id <task-id> \
  -recover-order <broker-order-id> -recover-tzid <verified-tzid>
```

Recovery checks the operation country and service and prevents binding one provider operation to multiple broker orders. This command is deliberately not exposed as an Agent tool. The service will close a recovered order if its original deadline has passed. Do not edit or delete state to force a retry: that discards the idempotency and budget history.

## Later: real provider configuration

Store `ONLINESIM_API_KEY` in the service's secret environment, enable API access in OnlineSim and configure its IP allowlist as appropriate. API keys are sent in the `Authorization: Bearer` header, not URLs. Network errors and malformed provider data are converted to safe error codes without returning request URLs, credentials or raw error bodies. Redirects and connection-reuse retries are disabled because OnlineSim uses GET even for non-idempotent operations.

```sh
./onlinesim-mcp -mode live -allow-live-orders -transport http \
  -price-unit <your-profile-tariff-currency> -data-dir ./data/live
```

This explicitly enables paid purchases. Supplying an API Key does not change the default mock mode. OnlineSim does not support SMS from payment services, banks or other financial institutions. Free shared numbers and long-term rentals are outside this implementation.

## Verification scope

Tests use deterministic mock providers and loopback HTTP servers. They cover normal/next-message flows, no stock, quote/attempt/budget limits, concurrent idempotency, ambiguous purchase and pointer results, process restart, durable intent, state locking/permissions, task isolation, protective intervals, deadline cleanup, HTTP 200 business errors, numeric/string responses, redirect blocking, non-replay of GET purchases, MCP handshake/tools and stdio identity.

The root module also has an opt-in integration test that runs the built executable through ARTEX's own HTTP and stdio connectors:

```sh
# From the repository root, after building the executable above:
ARTEX_PG_DSN='postgres://test:test@127.0.0.1:1/test?sslmode=disable&connect_timeout=1' \
ONLINESIM_MOCK_BINARY="$PWD/services/onlinesim-mcp/onlinesim-mcp" \
go test ./server -run 'Test(MCPTaskIdentityTemplate|MCPFixedIdentityIsPreserved|OnlineSimMCPMockIntegration)' -count=1 -v
```

The deliberately unreachable local PostgreSQL fixture prevents the root test harness from touching an existing database. The integration subprocess explicitly selects mock mode and clears the provider API Key.

No real API Key, paid purchase, live India inventory, refund, website registration or authenticated-session reuse has been verified. Website workflows and session management are the next layer once a target is selected.

API reference: https://onlinesim.io/openapi_docs/Onlinesim-API-UN/info/
