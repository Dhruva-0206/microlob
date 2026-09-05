# microlob — market microstructure simulation lab

## What this is

This is a market microstructure simulation lab, not just a matching engine. The
core is a limit order book engine written in Go (module `orderbook-engine`),
intended to eventually be surrounded by simulated trading agents (market
makers, noise traders, momentum/arb strategies) that submit orders against it,
plus a Python layer for quantitative analysis of the resulting market data
(spread dynamics, price impact, order flow statistics, etc.). Right now only
the Go engine's foundational data structures and matching logic exist — no
agents, no persistence, no Python layer yet. This is also an active
learning/portfolio project for the author, so code should stay clean,
idiomatic, and well-commented on the *why*, not just the *what*.

## Project structure

```
orderbook-engine/          Go module root (go.mod: module orderbook-engine)
  internal/orderbook/
    order.go                Order, Side (Buy/Sell), OrderType (Limit/Market)
    price_level.go           PriceLevel — FIFO queue of orders at one price
    orderbook.go             OrderBook — per-symbol book, SubmitOrder/AddOrder/CancelOrder/BestBid/BestAsk
    trade.go                 Trade — a single match between an incoming and a resting order
    orderbook_test.go         table-driven tests: FIFO ordering, best bid/ask, cancel, validation
    matching_test.go          table-driven tests: matching/fills/partial fills/market orders
```

## What's implemented so far

- `Order`: ID, Side, Price (int64 ticks), Quantity (int64), Timestamp
  (time.Time), OrderType (Limit/Market).
- `PriceLevel`: FIFO queue of orders resting at one price, backed by
  `container/list` for O(1) push-to-back and O(1) removal-by-handle.
- `OrderBook`: per-symbol book with a `map[int64]*PriceLevel` per side (bids,
  asks) plus a sorted `[]int64` of prices per side, giving O(1) `BestBid`/
  `BestAsk` reads.
- `SubmitOrder(order Order) ([]Trade, error)` — **the entrypoint for new
  orders**. Matches against the resting book first (price-time priority: the
  resting order's price wins, earliest-arrived order at a price trades
  first), generating `Trade`s, then rests any leftover quantity (Limit
  orders only — Market orders never rest; unfilled market quantity is
  dropped).
- `AddOrder(order Order) error` — internal helper, used by `SubmitOrder` to
  rest leftover Limit quantity, and by tests to seed resting book state
  directly. Not the entrypoint for new order flow anymore.
- `CancelOrder(orderID string) error` — removes a resting order by ID from
  wherever it is, regardless of whether it got there via `AddOrder` directly
  or as `SubmitOrder` leftover.
- No concurrency handling yet. No persistence. No agents. No Python layer.

## Key architectural decisions (locked in, with why)

- **Prices are `int64` ticks, never `float64`.** Float rounding error
  (0.1 + 0.2 != 0.3) is unacceptable when prices are map keys and drive
  matching/comparison decisions. Whatever layer talks to the outside world
  converts external prices into ticks; this package only ever sees integers.
  Quantities are `int64` for the same reason.
- **Best bid/ask: `map[int64]*PriceLevel` + a sorted `[]int64` per side, not
  a heap or a balanced tree, chosen deliberately for v1 simplicity.** A plain
  map has no ordering (O(n) scan to find the best price). A heap gives
  O(log n) insert and O(1) peek-at-top but doesn't cleanly support removing
  an *arbitrary* price level when it empties out (which can happen anywhere
  in the heap, not just at the top) without an extra price→index map kept in
  sync on every sift. The sorted-slice approach makes `BestBid`/`BestAsk`
  trivially O(1) with zero extra indices, at the cost of O(log n) binary
  search + O(n) shift on insert/remove. **A red-black tree / skip list is
  the natural next optimization if profiling shows the O(n) shift matters
  under deep books with heavy price-level churn — deliberately deferred, not
  implemented, and should be benchmarked against the current approach before
  being introduced, not assumed to be better.**
- **`container/list` (doubly-linked list) for each `PriceLevel`'s FIFO
  queue, not a slice.** Cancellation needs to remove an order from anywhere
  in the queue (front, back, or middle) in O(1) given a handle, which a
  slice can't do without an O(n) shift. `OrderBook.orderIndex` keeps a
  `map[string]*orderLocation` (side, price, `*list.Element`) per order ID so
  cancellation goes straight to the right place — O(1) — instead of
  scanning.
- **Matching is price-time priority, resting order's price wins.** An
  aggressor that would have paid more (or accepted less) still trades at
  the resting order's price — see `Trade`'s doc comment in `trade.go`.
- **Trade IDs** are a simple per-book monotonic counter
  (`"<symbol>-TRD-<n>"`), not UUIDs — deterministic and sufficient for a
  single-book, single-process engine at this stage.

## Conventions

- No floating point for prices or quantities, anywhere in this package.
  `int64` ticks only.
- New logic needs table-driven tests (`go test`'s standard `[]struct{...}`
  table pattern, `t.Run` per case). See `matching_test.go` for the current
  style — a table of setup orders + one order under test + expected trades +
  expected best bid/ask, with an optional `check func(t, *OrderBook)` field
  for scenario-specific extra assertions.
- Before considering any change done: run `go test ./...` and
  `go vet ./...` from the module root and confirm both are clean.
- Comments should explain *why* a data structure or approach was chosen, not
  restate what the code does — identifiers should already make the "what"
  obvious.

## Explicitly NOT doing yet (deliberately deferred — don't add unprompted)

- No red-black tree / skip list / order-statistics tree for price indexing.
  The sorted-slice approach is a known, intentional placeholder to be
  benchmarked against, not a gap to silently "fix."
- No Kafka or any external message bus/queue.
- No concurrency handling (mutexes, channels, goroutine-safety) in
  `OrderBook` yet.
- No persistence layer (database, snapshotting, WAL).
- No simulated trading agents yet.
- No Python quant analysis layer yet.

If a task seems to call for any of the above, confirm with the user before
introducing it rather than assuming it's the next logical step.

## Keeping this file current

Treat this file as living documentation. After any change that adds a type,
changes an architectural decision, or moves a boundary (e.g. what's the
"entrypoint" for something), update the relevant section above in the same
change — don't let it drift out of sync with the code.
