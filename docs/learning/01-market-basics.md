# 1 — How a market actually works

## The idea

A market is a continuous auction. Nobody sets "the price". At any moment there
are people willing to buy and people willing to sell, and a price exists only
where those two groups meet.

## The analogy

You are selling a phone. Three people have said what they will pay, and two
other sellers have said what they want:

```
Buyers                  Sellers
Ana     480             Marta   500
Luis    475             Sara    510
Pedro   460
```

Nothing has happened yet. Ana is the **best bid** at 480: the most anyone will
pay right now. Marta is the **best ask** at 500: the least anyone will accept.
The 20 between them is the **spread**.

A trade happens when somebody stops waiting and accepts the other side's price.

- If you want to buy *now*, you pay Marta's 500. You **buy at the ask**.
- If you want to sell *now*, you take Ana's 480. You **sell at the bid**.

That asymmetry is not a fee and nobody is cheating you. It is what "immediately"
costs.

## Waiting instead of paying

Rather than accept 500, you can publish your own offer: "I will buy, but only
at 490 or less." Your order joins the buyers and you become the best bid:

```
Buyers
You     490
Ana     480
Luis    475
```

Nothing happens until a seller decides 490 is acceptable. You have traded
certainty for a better price. That is a **limit order**.

## Who gets filled first

Exchanges normally use **price-time priority**: the best price wins, and among
orders at the same price the one that arrived first wins.

```
Ana  buys 2 at 490, sent 10:00
Luis buys 3 at 490, sent 10:01
```

A seller of two contracts fills Ana completely. Luis keeps waiting.

## Where the Go types are

Praxis does not model a full book. It observes only the top of it:

```go
type Quote struct {
    Instrument Instrument
    Time       LogicalTime
    Bid, Ask   Ticks
    BidSize    Qty
    AskSize    Qty
}
```

See [`internal/market/types.go`](../../internal/market/types.go).

## Common mistakes

- **"The price is 490."** There is no single price. There is a bid, an ask, and
  the price of the last trade — three different numbers.
- **"A limit order guarantees a fill."** It guarantees a price, not a fill. It
  may never execute.
- **"A tight spread means it's safe."** A spread tells you the cost of
  immediacy, nothing about direction.

## What Praxis simplifies

- Only the best bid and best ask, with their sizes. No depth behind them.
- No time priority and no queue position: your order is not competing with
  anyone in the simulation.
- No market impact: your own order never moves the book.

Each of these makes the simulation *optimistic* about size. That is why
execution caps a fill at the size actually displayed — see
[3 — Orders and fills](03-orders-and-fills.md).
