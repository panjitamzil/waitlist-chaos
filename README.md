# waitlist-chaos

**When the health check is green, but the business result is wrong.**

A small chaos engineering experiment for a paid waitlist. It shows how workers can crash, restart, and finish, leave an empty queue and a green health check, and still produce a result that breaks the rules users care about.

## Overview

Popular events sell out fast. Ticket platforms often keep a queue for people who missed out: [DICE](https://dicefm.zendesk.com/hc/en-gb/articles/4409669441169-Buy-tickets-to-a-sold-out-event-via-the-wait-list) has a waiting list for sold-out shows, and [Glastonbury](https://www.glastonburyfestivals.co.uk/news/2025-ticket-resale-faq/) runs a resale for tickets that were not paid for in time.

This repo uses a simpler model inspired by those flows. It is **not** how either platform works:

- People **pay when they join** the waitlist.
- If a confirmed seat is cancelled before the deadline, the first person in the queue gets it.
- At the deadline, free seats go to the next people in the queue. Everyone else gets a refund.

Now imagine a worker saves a conversion to the database and then crashes before it marks the job as done. The job is delivered again. The worker restarts, runs again, and exits normally. When the run is over, the queue is empty and the health check is green.

But the event now has more confirmed seats than it can hold.

The question this repo asks:

> Can a system look healthy after recovery while breaking the rules that matter to its users?

In these experiments, yes. A separate **business-invariant oracle** reads the final database state and checks the rules directly, instead of trusting worker exit codes.

## The problem

There are two workers:

- The **conversion worker** gives a free confirmed seat to the first waitlist entry after a cancellation.
- The **deadline worker** finalises the event at the deadline: it converts entries for any free seats and refunds the rest.

Both can crash and restart. Jobs can be delivered more than once.

The rules sound simple. A retry must not give away a second seat. Once the deadline is final, a late conversion job must not change the result. No one should get a seat and a refund. No one should be left with neither.

These rules break easily when database writes, job acknowledgements, and worker runs are not coordinated. A worker exit code tells you the worker finished. It does not tell you whether you oversold, paid someone twice, or left someone waiting.

## System overview

PostgreSQL stores the event, allocations (confirmed seats and waitlist entries), conversion jobs, and a simulated refund ledger. The experiment runner seeds each run, starts the workers as separate processes, injects faults at named points, and then checks the result.

```mermaid
flowchart TD
    A[Experiment runner] --> B[Conversion worker]
    A --> C[Deadline worker]
    B --> D[(PostgreSQL)]
    C --> D
    A --> E[Health check]
    D --> F[Business-invariant oracle]
    E --> G[Results]
    F --> G
```

The health check and the oracle answer different questions:

| Check | Question |
| --- | --- |
| Infrastructure health | Did the last run of each worker exit with code 0, and are there no pending, processing, or failed jobs? |
| Business-invariant oracle | Is the final state valid under the waitlist rules? |

Both checks run **once, after all workers have stopped**. The health check does not watch the system during the run. A crash that a restart recovered is counted, but it does not turn the final health result red. This is a simple stand-in for "the job queue drained and nothing is crashing anymore".

## Experiment design

Every run creates a fresh event, so earlier runs stay in the database for inspection.

Most scenarios start with a sold-out event: capacity 5, 5 confirmed seats, 5 people on the waitlist (`w1` to `w5`, in FIFO order). One confirmed seat is cancelled, so the correct result is **1 conversion and 4 refunds**.

The baseline (`s0`) and the deadline crash (`s4`) start with only 3 of 5 seats taken. There the correct result at the deadline is **2 conversions and 3 refunds**.

Each scenario runs against two implementations:

| Version | How it works |
| --- | --- |
| `naive` | Splits key writes across transactions, uses a random job key per delivery, does not recount free seats, and does not check whether the deadline is already final |
| `safe` | Does the work in one transaction, uses stable idempotency keys and row locks, and re-checks the current state before changing anything |

The bugs in `naive` are **written on purpose**. They give the oracle known failures to find. `safe` shows the same scenarios handled correctly.

### How faults are injected

Each worker has named fault points. The runner can tell a worker to **crash** (exit code 137) or **pause** (wait for `SIGUSR1`) at one of them. A pause lets the runner start another worker before resuming the first one.

This makes the races repeatable. We do not run the test hundreds of times and hope two workers overlap at the right moment.

## Business rules we check

| ID | Type | Rule |
| --- | --- | --- |
| S-1 | Safety | Active confirmed seats must not exceed capacity. |
| S-2 | Safety | A user holds at most one active confirmed seat. |
| S-3 | Safety | A waitlist entry is converted **or** refunded, never both. |
| S-4 | Safety | Conversions follow FIFO order. |
| S-5 | Safety | A waitlist entry has at most one completed refund. |
| L-1 | Liveness | After the deadline and a grace period, every waitlist entry has a seat or a refund. |

Safety rules describe states that must never happen. The liveness rule describes work that must eventually finish:

- Before the deadline: `NOT_APPLICABLE`
- Within the grace period: `NOT_YET_JUDGEABLE`
- After the grace period: checked

## Scenarios

| Scenario | Fault | Expected `naive` | Expected `safe` |
| --- | --- | --- | --- |
| `s0` | None | PASS | PASS |
| `s1` | Conversion worker crashes after commit, before acknowledging the job; then restarts | S-1: over capacity | PASS |
| `s2` | The same cancellation is delivered twice | S-1: over capacity | PASS |
| `s3` | A late conversion job runs while the deadline worker is paused between conversions and refunds | S-1 and S-3: over capacity, and one user gets a seat and a refund | PASS |
| `s4` | Deadline worker crashes after marking the event processed, before converting or refunding; then restarts | L-1: all waitlist entries left unresolved | PASS |

## Results

Recorded on 2026-11-09 with Go 1.27.0 against the local Docker database (PostgreSQL 17). `make experiment` was run three times in a row and produced the same table each time:

![make experiment output: all ten runs have HEALTH GREEN, four naive runs fail the oracle](docs/images/results.png)

<details>
<summary>Same output as text</summary>

```text
SCENARIO  VARIANT  HEALTH  ORACLE              VIOLATED  EXPECTED                  RESULT
s0        naive    GREEN   PASS                -         PASS                      MATCH
s0        safe     GREEN   PASS                -         PASS                      MATCH
s1        naive    GREEN   SAFETY_VIOLATION    S-1       SAFETY_VIOLATION S-1      MATCH
s1        safe     GREEN   PASS                -         PASS                      MATCH
s2        naive    GREEN   SAFETY_VIOLATION    S-1       SAFETY_VIOLATION S-1      MATCH
s2        safe     GREEN   PASS                -         PASS                      MATCH
s3        naive    GREEN   SAFETY_VIOLATION    S-1,S-3   SAFETY_VIOLATION S-1,S-3  MATCH
s3        safe     GREEN   PASS                -         PASS                      MATCH
s4        naive    GREEN   LIVENESS_VIOLATION  L-1       LIVENESS_VIOLATION L-1    MATCH
s4        safe     GREEN   PASS                -         PASS                      MATCH

s1/naive (event 2f917b88-9648-451a-b3a1-e8cad51f9e46, crashes recovered: 1)
  - S-1 6 active confirmed seats exceed capacity 5

s2/naive (event febc6a81-8ec8-4738-bb14-baa18261e471, crashes recovered: 0)
  - S-1 6 active confirmed seats exceed capacity 5

s3/naive (event 4d22f606-36dc-489d-9387-4e8f18f15522, crashes recovered: 0)
  - S-1 6 active confirmed seats exceed capacity 5
  - S-3 w2 received a confirmed seat AND a refund

s4/naive (event ce0b83c2-efc8-464d-a425-8f7eb40114a3, crashes recovered: 1)
  - L-1 5 waitlist entries still unresolved (no seat, no refund): w1 w2 w3 w4 w5
```

</details>

The screenshots come from a later run of the same commands, so their event IDs differ from the text. To make them again, run `make screenshots` (needs `vhs` and `ffmpeg`).

`MATCH` means the run produced the designed outcome. It is not a discovery: the `naive` bugs were put there on purpose.

What the table shows is the gap between the two checks. The final health check is green in all ten runs. The oracle finds a broken result in four of them.

### A closer look at `s3`

The deadline worker and a late conversion job both act on the same waitlist:

1. The `naive` deadline worker marks the event as processed and plans: 1 free seat, convert `w1`, refund `w2` to `w5`.
2. It converts `w1`, then pauses before the refunds.
3. A late conversion job (from a cancellation made before the deadline) runs. It does not check the deadline or recount seats, so it converts `w2`.
4. The deadline worker resumes and follows its old plan, including the refund for `w2`.

```mermaid
sequenceDiagram
    participant D as Deadline worker
    participant C as Conversion worker
    participant DB as PostgreSQL
    D->>DB: Mark processed, convert w1
    Note over D: Pause before refunds
    C->>DB: Convert w2
    Note over C: Job done, exit 0
    Note over D: Resume
    D->>DB: Refund w2, w3, w4, w5
    Note over DB: 6 confirmed seats, w2 also refunded
```

The oracle on that event:

![oracle output for s3 naive: S-1 and S-3 violated](docs/images/s3-naive.png)

<details>
<summary>Same output as text</summary>

```text
$ ./bin/oracle -event 4d22f606-36dc-489d-9387-4e8f18f15522
event 4d22f606-36dc-489d-9387-4e8f18f15522: SAFETY_VIOLATION
  S-1  VIOLATION  active confirmed seats <= capacity
                  - 6 active confirmed seats exceed capacity 5
  S-2  PASS       at most one active confirmed seat per user
  S-3  VIOLATION  a waitlist entry is converted or refunded, never both
                  - w2 received a confirmed seat AND a refund
  S-4  PASS       conversions follow FIFO order
  S-5  PASS       at most one completed refund per entry
  L-1  PASS       every waitlist entry is resolved after the deadline
```

</details>

Both workers exited with code 0, and no job was left behind. The final health check is green.

The `safe` version of the same scenario:

![s3 safe log: the conversion worker waits for the lock, then makes no change](docs/images/s3-safe.png)

<details>
<summary>Key lines as text (timestamps removed)</summary>

```text
[deadline-worker#1]   event 45de4187: plan free_seats=1 convert=[w1] refund=[w2 w3 w4 w5]
[deadline-worker#1]   FAULT_PAUSED deadline_after_conversions
[conversion-worker#2] claimed job 7bf49df8 (attempt 1)
conversion worker still running (blocked, e.g. on the event row lock)
[deadline-worker#1]   FAULT_RESUMED deadline_after_conversions
[deadline-worker#1]   event 45de4187: finalised (1 converted, 4 refunded)
[conversion-worker#2] job 7bf49df8: no-op: deadline already finalised, its outcome is final
```

</details>

The conversion worker waits for the event row lock. When it gets the lock, the deadline is already final, so it changes nothing.

## What changed in the safe version

| Concern | `naive` | `safe` |
| --- | --- | --- |
| Conversion | Assumes one cancellation means one free seat | Locks the event row and counts active seats again |
| Deadline | Ignores whether the deadline is final | No-op if the deadline is already final |
| Write and job acknowledgement | Separate transactions | One transaction |
| Job idempotency key | Random per delivery | `cancellation:<allocation id>` |
| Refunds | No status check, random key | `WHERE status = 'active'` and `refund:<allocation id>` |
| Deadline finalisation | Marks processed first, then separate steps | One transaction under the event row lock |

Restarting workers is not enough on its own. A retry or a late job must not be able to create an invalid result.

## What I learned

1. **Pick exact fault points.** A crash right after a commit tells you much more than a crash at a random moment.
2. **Control the order of events.** Pause one worker, run another, then resume the first. The race becomes easy to repeat.
3. **Judge the result after the work settles.** A half-finished workflow is not always a bug. Judge the final state.
4. **Keep the oracle separate.** It reads the database and applies the rules. It does not trust the workers' own success messages.
5. **Do not stop at a green health check.** A system can recover from a crash and still leave users with the wrong result.

This does not make infrastructure monitoring useless. It means you also need checks that know what a correct outcome looks like.

## Run it locally

### Requirements

- Go 1.27 or newer (see `go.mod`)
- PostgreSQL 15 or newer: local Docker or Supabase

`DATABASE_URL` is read from the environment first, then from `.env`. A variable set in your shell wins over `.env`.

### Local Docker (recommended)

```sh
make db-local-up      # PostgreSQL 17 on localhost:5433
export DATABASE_URL='postgresql://chaos:chaos@localhost:5433/waitlist_chaos?sslmode=disable'
make build
make migrate-up
make experiment
```

### Supabase

```sh
cp .env.example .env  # then set DATABASE_URL in .env
make build
make migrate-up
make experiment
```

Use the session pooler connection string (port `5432` on `*.pooler.supabase.com`). The direct connection is IPv6-only, and the transaction pooler does not support the session behaviour this project needs.

### Useful commands

```sh
make experiment-verbose                        # all runs, with worker logs
./bin/experiment -scenario s3 -variant naive -v
./bin/oracle -event <event-id>                 # check one event
./bin/oracle -event <event-id> -json
```

`make experiment` exits with a non-zero code if any run does not match its expected outcome.

### Tests

```sh
make test               # unit tests, no database needed
make test-integration   # all tests, including the database tests, against DATABASE_URL
```

The integration tests run against a real database. They cover:

- **Store:** guarded conversions and refunds, refund ledger rows per key, job key deduplication, FIFO order, and job leases.
- **Workers:** naive redelivery going over capacity, safe acknowledgement in the same transaction, safe recount on redelivery, naive conversion after the deadline, safe no-op after the deadline, and safe deadline finalisation.
- **Scenarios:** all ten runs above.

They create new events on every run. Point them at the local Docker database, not a shared one.

## Project structure

```text
cmd/                  migrate, conversion-worker, deadline-worker, oracle, experiment
internal/domain       FIFO order and deadline plan (no database)
internal/store        SQL shared by workers, oracle, and runner
internal/fault        named crash and pause points
internal/worker       naive and safe conversion and deadline workers
internal/oracle       rule checks and database snapshot loader
internal/health       infrastructure health baseline
internal/experiment   scenarios, process runner, and report
migrations/           SQL schema
docs/demo             vhs tapes and the script that renders the screenshots
docs/images           screenshots used in this README
```

## Limitations

This is a small, controlled experiment, not a production ticketing system.

- **The bugs are added on purpose.** The oracle catches known bugs. This says nothing about how often such bugs happen in real systems.
- **The health check is simplified.** It runs once at the end and only looks at exit codes and the job queue. Real teams may already track business metrics that would catch some of these cases.
- **Payments are simulated.** Refunds are rows in a ledger table. No payment provider is involved.
- **The waitlist model is simplified.** It is inspired by real ticket queues but does not copy any platform's rules.
- **Fault points are chosen by hand.** The experiment does not try every possible worker order. Property-based testing or model checking could explore more.
- **Only worker faults are covered.** Database and network failures are out of scope.

## Related work

Checking business rules during fault injection is not a new idea.

[FaultForge](https://doi.org/10.1145/3832783.3834552) (Yang et al., ASE 2026) is a broader framework. It derives fault cases from business invariants and, on an extended Train-Ticket microservice system, validates faults with business-layer evidence, plus technical-layer and cross-layer evidence for faults caused by infrastructure.

This repo is much smaller. It has one waitlist workflow, hand-written faults, and a runner that controls the order of worker actions. It is meant as a case study you can run and read in one sitting, not a general chaos testing framework.

## License

MIT. See [LICENSE](LICENSE).
