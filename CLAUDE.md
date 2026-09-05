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
    engine.go                 Engine — single-writer-goroutine concurrency wrapper around OrderBook
    engine_test.go             basic Engine tests + concurrency stress test (run with -race)
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
- `Engine`: a concurrency-safe wrapper around one `OrderBook`, using the
  single-writer-goroutine pattern rather than a mutex — see the dedicated
  section below. Exposes `SubmitOrder`/`CancelOrder`/`BestBid`/`BestAsk`
  with the same signatures as `OrderBook`'s (`BestBid`/`BestAsk` drop the
  error and just return `(0, false)` once stopped), plus `Start()`/`Stop()`
  for lifecycle control. Channel plumbing is entirely hidden from callers.
- No persistence. No agents. No Python layer yet.

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
- **Concurrency: `Engine` uses a single-writer goroutine, not a mutex around
  `OrderBook`.** `Engine` owns one `OrderBook` and runs it inside exactly one
  goroutine (`run`, started by `Start`). Every public method
  (`SubmitOrder`/`CancelOrder`/`BestBid`/`BestAsk`) packages its arguments
  into a small request struct with its own reply channel, sends it on an
  unbuffered `requests` channel, and blocks on the reply. `run` is the only
  goroutine that ever touches the `OrderBook`, so no lock is needed — this
  is Go's "share memory by communicating" idea applied literally: instead of
  many goroutines coordinating access to shared state with a `sync.Mutex`,
  they hand work to the one goroutine that owns it and get an answer back on
  a private channel. The channel is unbuffered on purpose: a caller's send
  only completes once `run` is ready to receive it, so requests are
  serialized in true arrival order with no separate queue, and a busy owning
  goroutine applies backpressure to callers automatically.
  Each request variant (`submitOrderRequest`, `cancelOrderRequest`,
  `bestBidRequest`, `bestAskRequest`) is its own small type implementing an
  `engineRequest` interface (`execute(book *OrderBook)`), rather than one
  struct with a "kind" tag and a pile of mostly-unused fields — the four
  operations have genuinely different payloads and response shapes, so
  method dispatch reads better here than a switch on an enum.
  Shutdown uses `context.Context`: `Stop()` cancels the context and blocks
  until `run` has actually exited (a `stopped` channel it closes on the way
  out), so `Stop()` returning is a real guarantee the book is quiescent, not
  just a signal that it will be soon. Requests sent concurrently with `Stop`
  either complete normally or get `ErrEngineStopped` — never a panic or a
  hang.
  **This will be benchmarked against a mutex-protected `OrderBook` later, as
  part of the portfolio's performance comparison. The mutex-based
  alternative is deliberately not implemented yet — don't add it unprompted;
  the point of the comparison is to measure the single-writer-goroutine
  design against it once both exist, not to assume one wins.**

## Conventions

- No floating point for prices or quantities, anywhere in this package.
  `int64` ticks only.
- New logic needs table-driven tests (`go test`'s standard `[]struct{...}`
  table pattern, `t.Run` per case). See `matching_test.go` for the current
  style — a table of setup orders + one order under test + expected trades +
  expected best bid/ask, with an optional `check func(t, *OrderBook)` field
  for scenario-specific extra assertions.
- Before considering any change done: run `go test ./...` and
  `go vet ./...` from the module root and confirm both are clean. Any change
  touching `Engine` or other concurrent code must also pass
  `go test -race ./...` — a plain `go test` run can pass even with a broken,
  racy design, since a data race is a matter of timing, not correctness of
  output on any single run. Note: `-race` requires cgo, which needs a C
  compiler on PATH (this machine uses a MinGW-w64 gcc installed via winget
  for that purpose) — if it's missing, `go test -race` fails immediately
  with `requires cgo`, not with a race finding.
- `Engine` lives in `internal/orderbook` rather than its own subpackage: it
  wraps `OrderBook` tightly enough (its requests carry `Order`/`Trade`
  values, its `execute` methods take `*OrderBook` directly) that splitting
  it out would just add an import and a stutter (`orderbook.Engine` calling
  into an `orderbook`-shaped API) with no real encapsulation benefit at this
  package's current size. Revisit if the package grows enough that this
  stops being true.
- Comments should explain *why* a data structure or approach was chosen, not
  restate what the code does — identifiers should already make the "what"
  obvious.

## Explicitly NOT doing yet (deliberately deferred — don't add unprompted)

- No red-black tree / skip list / order-statistics tree for price indexing.
  The sorted-slice approach is a known, intentional placeholder to be
  benchmarked against, not a gap to silently "fix."
- No Kafka or any external message bus/queue.
- No mutex-based concurrency alternative to `Engine`'s single-writer
  goroutine — deliberately deferred until there's a single-writer
  implementation to benchmark it against (there now is: see `Engine` in the
  architectural decisions above). Don't add a `sync.Mutex`-protected
  `OrderBook` variant unprompted.
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
