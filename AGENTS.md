# AGENTS.md — Praxis

Read this file fully. It is short on purpose. Everything here is
non-negotiable. `docs/PRAXIS_SPEC.md` is the normative source for reasoning,
domain semantics and the roadmap; consult it before proposing architecture.
If the documents disagree, this file wins for working rules.

## What Praxis is

Praxis is a deterministic trading simulation engine that measures and trains
trader behaviour. It replays market sessions, applies prop-firm-style
evaluation rules, and records every meaningful user decision as a domain
event.

Praxis does not predict markets, generate trading signals, or present or imply
expected returns. If a proposal drifts toward forecasting, signal generation,
automated trading, or performance promises, stop and say so.

## The seven rules

1. No binary floating-point for domain prices, money, P&L, commissions, risk
   limits, or drawdown. Use integer domain types such as `Ticks`, `Cents`,
   `Qty`, and `LogicalTime`. Floating-point is permitted only at explicitly
   marked analytics or random-sampling boundaries and must never feed monetary
   or risk logic.
2. No `time.Now()` in domain code. Time enters as data.
3. No global `math/rand`. Randomness enters through an injected `Randomizer`;
   its explicit seed is recorded in `SessionStarted`.
4. No `map` iteration where ordering affects results. Use ordered slices or
   sort explicitly with a documented tie-breaker.
5. The kernel runs in one goroutine. Concurrency is allowed only in adapters,
   behind ports. Inputs crossing into the kernel have a deterministic order.
6. Execution never grants favourable price improvement. Market buys fill at
   or above the ask and market sells at or below the bid. Limit and stop orders
   follow the conservative rules in `docs/PRAXIS_SPEC.md`; stops never fill
   better than their level, gaps resolve against the trader, and ambiguous bars
   resolve to the stop.
7. Domain packages—whether under `domain/` or later under `internal/`—import
   no infrastructure: no provider SDKs, HTTP clients, database drivers, or
   filesystem access.

## Conflicts and disagreement

For domain behaviour, use the most recent explicit ADR or user instruction.
When code, tests, this file, and the specification disagree, do not silently
choose: report the contradiction. This file wins for working rules.

If an instruction appears technically wrong, say so once and briefly, with a
concrete reason, then follow it unless it is unsafe, impossible, internally
contradictory, or conflicts with a higher-priority instruction. In those cases,
stop and state the blocker. Do not silently implement something else.

## Working method

Use tests first for execution, portfolio/account, risk, and challenge changes:

1. Restate the behaviour.
2. Name the invariants.
3. List edge cases.
4. Write failing tests.
5. Implement the minimum.
6. Refactor with the suite green.

Do not add unrequested scope. Do not create an abstraction before a second
implementation exists, except for the ports explicitly exempted in the spec.
Never weaken a correct invariant to make an implementation pass.

When a property test fails, assume the implementation is wrong first. Do not
weaken or delete the invariant to obtain a green suite. If the test itself is
suspected, demonstrate precisely how it misrepresents the documented invariant
before changing it.

Be a demanding reviewer. Do not praise code merely because it compiles. Say
when a statistic lacks a sufficient sample, a design is aesthetics posing as
engineering, or an idea is weak.

## Before claiming code works

For changes to Go code, run these commands individually from the repository
root, in this order:

1. `go build ./...`
2. `go vet ./...`
3. `go test ./...`
4. `go test -race ./...`

Report each command and its actual result. If one fails, report the failure and
continue only when later checks remain meaningful. If commands cannot be run,
say so explicitly. Never report output that was not actually produced.

## Definition of done

For behavioural changes: behaviour is defined; tests cover relevant edge
cases; implementation is minimal; invariants remain intact; build, vet, tests,
and race tests pass; no unrelated scope was added.

For documentation-only changes, tests are required only when the documentation
contains executable examples or claims about repository behaviour. Do not
claim a historical test result as a current result.

Update an existing ADR or create one only when a durable architectural decision
changes—not for ordinary implementation detail or refactoring.

At completion report: files changed, domain decisions introduced, tests added,
commands run with real results, remaining edge cases, and the next smallest
vertical slice.
