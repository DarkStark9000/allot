# Design decisions

Each decision names the alternative it rejected and why.

## 1. The state machine is a pure function, and its side effects are data

`order.Apply(order, event)` returns the next order, its ledger postings, and its outbox messages. It performs no I/O. `store.Tx.Apply` writes all of it in one transaction.

- **Rejected:** methods on an order service that call the database and the exchange as they go.
- **Why:** a pure core can be tested exhaustively. The table test covers every state against every event, and the fuzz test drives one order through any sequence of events and checks the ledger after each step. The database code then has one job: write one result atomically.

## 2. A transactional outbox with leases, not calls inside the transaction

The message to the exchange is a row written with the state change. A relay claims due rows with `FOR UPDATE SKIP LOCKED` and a lease, commits the claim, makes the call, and then marks the row sent in the same transaction as the event the answer produced.

- **Rejected:** calling the exchange inside the database transaction. A slow exchange would hold row locks, and a crash after the call but before the commit would lose the answer.
- **Rejected:** two-phase commit. The exchange, the gateway, and the registrar do not take part in one.
- **Cost:** a message can be sent more than once. Every call names the order by a stable reference, so a second send is harmless.

## 3. A timeout is a state, not a failure

When the exchange does not answer, the order moves to `submit_uncertain` and the relay asks for the order's status by its reference. If the exchange has never seen the order, the relay sends it again under the same reference.

- **Rejected:** treating a timeout as a rejection and refunding. The exchange may have accepted the order, so the investor would get both the units and the money back.
- **Rejected:** resubmitting under a new reference. That is how one tap becomes two orders.
- **Evidence:** both rejected designs are planted behind build tags, `plant_new_reference` and `plant_timeout_rejection`. Each one breaks guarantee G4 in every chaos run.

## 4. Evidence moves an order forward

A registrar file that shows units for an order proves the exchange accepted it, even if the acceptance message never came. The order moves to `allotted` from `submitted`, `submit_uncertain`, or `accepted`. A later message that contradicts a final outcome opens a finding instead of changing the order.

## 5. Plain SQL with pgx, no ORM and no code generator

About twenty queries live next to the code that uses them, in the `store` package.

- **Rejected:** an ORM. The queries are the subject of this project: `SKIP LOCKED`, `ON CONFLICT DO NOTHING`, partial unique indexes. Hiding them defeats the purpose.
- **Rejected for now:** `sqlc`. Its PostgreSQL parser needs cgo, which the author's Windows machine does not have, and twenty queries do not need generated code. Revisit if the count grows.

## 6. Money is fixed-point; units are truncated

Rupees are `int64` paise, NAV is in ten-thousandths of a rupee, and units are in thousandths of a unit. Units are amount over NAV, truncated. Nothing in the money path uses `float64`.

- **Note:** registrars round units by their own conventions. Reconciliation compares the file's units with this formula and opens a finding when they differ. It does not guess the registrar's rounding.

## 7. NAV rules are data, and v0.1 covers purchases only

`internal/navdate/rules.json` holds the cut-off rules: one per category, with the date it took effect and its source. Rules are not yet chosen by date; a history of rules per category is the next step. Redemptions have different rules and different exceptions, and they are out of scope until they can be modeled with the same care.

## 8. Tests use a real PostgreSQL, located by an environment variable

Each test gets its own schema in the database named by `ALLOT_TEST_DATABASE_URL`. CI runs PostgreSQL 18 as a service container and sets `ALLOT_REQUIRE_DB`, which turns a skipped database test into a failure.

- **Rejected:** mocks of the database. The behavior under test is the database's: unique constraints, row locks, and `SKIP LOCKED`.
- **Rejected:** testcontainers. A service container in CI and `docker compose` locally give the same result with fewer dependencies.

## 9. Authentication is out of scope

The API reads the caller's user ID from the `X-User-ID` header. A real service reads it from a verified token. Every query that returns an order still checks that the order belongs to the caller.
