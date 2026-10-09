# allot

[![ci](https://github.com/DarkStark9000/allot/actions/workflows/ci.yml/badge.svg)](https://github.com/DarkStark9000/allot/actions/workflows/ci.yml)

A mutual fund order engine in Go. Every payment, exchange answer, and registrar row changes an order once, while the payment gateway, the exchange, and the registrar retry, reorder, and go silent.

An investor taps Invest on a weak network. One purchase now crosses four systems that share no transaction, and each delivers messages at least once, in any order. The failures investors see are **two orders for one tap** and **money debited, units not allotted**. allot shows how a backend prevents both, and a chaos harness proves it by injecting each of these faults on every run.

Delivery between systems is at least once. The effect on an order happens once. This README says "effectively once" for that, never "exactly once".

## Guarantees

| | Guarantee | How |
|---|---|---|
| G1 | One order per idempotency key, per user. | Key table with a request fingerprint and the stored response, per the [IETF Idempotency-Key draft](https://www.ietf.org/archive/id/draft-ietf-httpapi-idempotency-key-header-07.html). A unique constraint behind it. |
| G2 | Each inbound event changes an order at most once. | Inbox table keyed by `(source, event_id)`, written in the same transaction as the change. |
| G3 | An order only moves along the state machine. | One pure `order.Apply`. Every move is recorded with the event that caused it. |
| G4 | Every rupee is accounted for, and the outside world agrees. | Double-entry ledger per order. The exchange and the gateway records must match the order's state. |
| G5 | Every order ends in a final state, or a finding asks a person to look. | Leased outbox, status queries, an expirer, and reconciliation. |
| G6 | The NAV date follows the cut-off rules. | `navdate`: rules as data, each with its source. |

## Proof

`go run ./cmd/chaos` places orders through the HTTP API with racing, retried, and reused keys. It pays them through a gateway that duplicates, delays, and reorders webhooks, and sometimes sends a failure after a success. It submits them to an exchange that rejects, drops, and goes silent after accepting. It kills relays mid-flight, expires unpaid orders while late payments are still on the way, and reconciles a registrar file with missing and wrong rows. Then it checks G1 to G6 against the database, the exchange, and the gateway.

Fifty runs, seeds 1 to 50, 200 orders each, every fault on:

```text
$ go run ./cmd/chaos -runs 50 -orders 200 -seed 1
50 runs, 10000 orders, 5m34s
requests by status    201=20280  409=9662  422=504
final states          accepted=394  allotted=7609  expired=471  payment_failed=417  refunded=1109
findings              missing_allotment=394  unit_mismatch=159
webhooks delivered    28893 (duplicates included)
exchange calls        9410 for 8736 exchange orders
refunds               1109
guarantee violations  0 (runs failed: 0)
```

Read it this way:

- **Requests:** 30,446 requests for 10,000 orders. Every racing and retried request either replayed its order (`201`) or waited (`409`), and every reused key with a new body was refused (`422`).
- **Webhooks:** 28,893 deliveries, duplicates included, never applied an event twice.
- **Exchange:** 9,410 calls created 8,736 exchange orders, one per submitted order. The extra calls were retries and resubmissions under the same reference.
- **Findings:** the 394 orders left in `accepted` are exactly the 394 whose registrar row was dropped. Each one has a `missing_allotment` finding, and none was refunded or allotted by guesswork. The 159 `unit_mismatch` rows were held back, not applied.

Measured on a laptop: AMD Ryzen AI 7 350, 23 GB RAM, Windows 11, Go 1.27.0, PostgreSQL 18 in Docker. CI repeats ten runs on Linux with the race detector on the test suite.

### The harness catches the bugs it is built to catch

Each row is a real bug planted in the relay, followed by one chaos run of 150 orders.

| Planted bug | Result |
|---|---|
| A new exchange reference on every attempt | 233 violations of G4: orders the exchange never recorded under their reference |
| A timeout treated as a rejection, then refunded | 15 violations of G4: refunds for orders the exchange accepted |

## Run it

You need Go 1.27 and Docker.

```bash
docker compose up -d --wait
export ALLOT_DATABASE_URL='postgres://allot:allot@127.0.0.1:55432/allot?sslmode=disable'
export ALLOT_TEST_DATABASE_URL="$ALLOT_DATABASE_URL"

go test ./...                                   # unit and integration tests
go test -run='^$' -fuzz=FuzzLifecycle ./internal/order
go run ./cmd/chaos -runs 10 -orders 200         # exit status 1 on any violation
```

`go run ./cmd/allotd` runs the API, two relays, and the expirer against a real exchange and gateway. Its configuration is in [`cmd/allotd/main.go`](cmd/allotd/main.go).

## How an order moves

```mermaid
stateDiagram-v2
    [*] --> payment_pending
    payment_pending --> submitted: payment succeeded
    payment_pending --> payment_failed: payment failed
    payment_pending --> expired: no payment in window
    submitted --> accepted: exchange accepted
    submitted --> submit_uncertain: no answer
    submitted --> refund_pending: exchange rejected
    submit_uncertain --> accepted: status is accepted
    submit_uncertain --> refund_pending: status is rejected
    submitted --> allotted: units in registrar file
    submit_uncertain --> allotted: units in registrar file
    accepted --> allotted: units in registrar file
    payment_failed --> refund_pending: money arrives late
    expired --> refund_pending: money arrives late
    refund_pending --> refunded: refund succeeded
    allotted --> [*]
    refunded --> [*]
```

- **A timeout is not a failure.** The order waits in `submit_uncertain` until a status query, under the same reference, says what happened.
- **Evidence moves forward.** Units in the registrar's file prove acceptance, even if that message never came.
- **Late money goes back.** A payment that arrives after the order failed or expired is refunded, never invested.
- **Contradictions go to a person.** An event that conflicts with what the order already knows opens a finding and changes nothing.

## Failure modes, and the test for each

| | What goes wrong | What allot does | Test |
|---|---|---|---|
| F1 | The app retries `POST /v1/orders` after a timeout | Returns the stored response; one order exists | `TestCreateOrder_ReplaysStoredResponse` |
| F2 | The same key arrives with a different body | `422` with a problem document | `TestIdempotency_KeyReuseDifferentBody` |
| F3 | Requests race with one key | One order; the others get it or `409` | `TestIdempotency_ConcurrentSameKey`, `TestIdempotency_InFlightIs409` |
| F4 | The success webhook arrives three times | Applied once; every delivery gets `200` | `TestInbox_DuplicateDeliveries` |
| F5 | A stale failure webhook follows a success | Recorded as stale, ignored | `TestPayment_StaleFailureAfterSuccess` |
| F6 | A relay dies between claiming and sending | The lease runs out; another relay sends it | `TestOutbox_CrashBetweenCommitAndSend` |
| F7 | The exchange accepts, then never answers | Status query; one exchange order | `TestExchange_TimeoutAfterAccept` |
| F8 | The exchange rejects a paid order | Refund; the ledger returns to zero | `TestRefund_LedgerBalances` |
| F9 | Units arrive before the acceptance | Moves straight to `allotted` | `TestAllotment_EarlyEvidence` |
| F10 | Money reaches the fund after the cut-off | Next business day's NAV | `TestNAVDate` |
| F11 | The registrar file is missing a row | A `missing_allotment` finding | `TestRecon_MissingAllotment` |
| F12 | The file's units differ from amount over NAV | A `unit_mismatch` finding; not applied | `TestRecon_UnitMismatch` |
| F13 | The database is unavailable | `503` with `Retry-After`; nothing written | `TestAPI_DatabaseDown` |
| F14 | Relays race for the same messages | Each message is claimed once | `TestOutbox_ConcurrentRelays` |

Also tested: an order the exchange never saw is sent again under the same reference (`TestExchange_DroppedThenResubmitted`), late money after expiry is refunded (`TestExpiryThenLatePaymentIsRefunded`), and a second, different payment for one order opens a finding (`TestWebhook_SecondPaymentOpensFinding`).

## API

| Method and path | Purpose |
|---|---|
| `POST /v1/orders` | Place an order. Needs `Idempotency-Key` and `X-User-ID`. Body: `{"scheme_code", "category", "amount_paise"}`. |
| `GET /v1/orders/{id}` | The order and its full history. |
| `POST /v1/webhooks/payments` | Gateway events, signed with HMAC-SHA256 in `Allot-Signature: sha256=<hex>`. |
| `GET /healthz` | `200` when the database answers. |

`X-User-ID` stands in for authentication; a real service reads the user from a verified token. Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem documents whose `type` links to a section below.

## NAV dates

The fund decides the NAV by when the money and the order both reach it, compared with a cut-off. allot holds these rules as data in [`internal/navdate/rules.json`](internal/navdate/rules.json), each with an effective date and its source, and treats a rule change as a data change with a test.

| Scheme | Cut-off (IST) | Received by the cut-off | Received after |
|---|---|---|---|
| Liquid and overnight | 1:30 PM | NAV of the day before receipt | NAV of the day before the next business day |
| All others | 3:00 PM | NAV of the day of receipt | NAV of the next business day |

A purchase received on a holiday counts as received at the start of the next business day. v0.1 covers purchases only. Platforms often set their own, earlier cut-offs so that money reaches the fund in time; those are a platform's choice and not modeled here.

## Layout

```text
cmd/allotd        the service: API, relays, expirer
cmd/chaos         the fault-injection harness
internal/order    states, events, Apply (pure)
internal/money    paise, NAV, and units as fixed-point integers
internal/navdate  NAV date rules, embedded as data
internal/ledger   double-entry postings
internal/store    PostgreSQL: orders, history, keys, inbox, outbox, findings
internal/httpapi  routes, idempotency, webhooks, problem documents
internal/outbox   the relay, leases, and backoff
internal/recon    registrar file checks and findings
internal/sweep    expiry of unpaid orders
internal/fake     a misbehaving exchange, gateway, and registrar
internal/chaos    one end-to-end run
internal/invariant  the checks for G1 to G6
```

Design decisions, with the alternatives rejected, are in [`docs/decisions.md`](docs/decisions.md).

## What it is not

- Not connected to any real exchange, gateway, or registrar. The fakes follow public descriptions of these systems, not private APIs or file formats.
- Not a description of how any company builds its systems.
- Not complete on regulation. The rules cite their sources and cover purchases only.

## Problem types

### problem-unauthenticated
The request has no `X-User-ID` header.

### problem-idempotency-key-required
`POST /v1/orders` needs an `Idempotency-Key` header of at most 255 characters.

### problem-idempotency-key-reused
The key was used before with a different request. Use a new key for a new request.

### problem-idempotency-key-in-use
A request with this key is still running. Retry after `Retry-After` seconds.

### problem-invalid-body
The body is not valid JSON for this endpoint, or has unknown fields.

### problem-invalid-order
The order failed validation: a missing scheme code, an unknown category, or an amount that is not positive.

### problem-body-too-large
The body is larger than 16 KiB.

### problem-bad-signature
The webhook's `Allot-Signature` does not match its body.

### problem-not-found
No order with this ID belongs to the caller.

### problem-unavailable
The database is unavailable. Retry with the same `Idempotency-Key`.

## License

Apache-2.0. Copyright 2026 Debarshi Das.
