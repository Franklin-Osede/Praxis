# Learning path

Written for someone who is building Praxis and learning futures trading at the
same time. Read in order; each document assumes the one before it.

1. [Market basics](01-market-basics.md) — order books, bid and ask, why buying
   immediately costs more than waiting.
2. [Futures and MNQ](02-futures-and-mnq.md) — what a contract actually is,
   leverage, and why one instrument was chosen.
3. [Orders and fills](03-orders-and-fills.md) — market, limit and stop orders,
   and the rule that Praxis never lies in the trader's favour.
4. [Positions, P&L and equity](04-positions-pnl-and-equity.md) — cost basis,
   weighted average cost, and why money is an integer.
5. [Prop firm challenges](05-prop-firm-challenges.md) — evaluation rules, what a
   trading day is, and what Praxis is actually measuring.

[Glossary](GLOSSARY.md) — every term in one place.

These documents explain. They are not normative: `AGENTS.md` holds the working
rules, `docs/PRAXIS_SPEC.md` the domain decisions, and `docs/adr/` the
architecture. If this path and those documents disagree, they win and this is a
bug.

The goal is to be able to explain the whole chain in your own words:

```
Quote -> Order -> Fill -> Position -> Account -> Equity -> Challenge
```
