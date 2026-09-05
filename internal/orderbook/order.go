// Package orderbook implements the core data structures for a limit order
// book: orders, per-price FIFO queues, and a per-symbol book that tracks the
// best bid/ask. This package currently only supports resting orders on the
// book — no matching engine yet.
package orderbook

import "time"

// Side indicates which side of the book an order belongs to.
type Side int

const (
	Buy Side = iota
	Sell
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "Buy"
	case Sell:
		return "Sell"
	default:
		return "Unknown"
	}
}

// OrderType distinguishes limit orders (rest on the book at a specified
// price) from market orders (execute immediately at the best available
// price). Only Limit is usable today since AddOrder has no matching logic —
// a Market order has no price to rest at, so AddOrder rejects it. The type
// exists now so the field doesn't need to be threaded through every call
// site later once matching is implemented.
type OrderType int

const (
	Limit OrderType = iota
	Market
)

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "Limit"
	case Market:
		return "Market"
	default:
		return "Unknown"
	}
}

// Order represents a single resting order.
//
// Price is expressed in integer "ticks" (the smallest price increment for
// the symbol) rather than float64. Financial quantities must compare and sum
// exactly — float64 arithmetic accumulates rounding error (e.g. 0.1 + 0.2 !=
// 0.3) which is unacceptable for prices used as map keys and for matching
// decisions. Using int64 ticks means price equality/ordering and quantity
// totals are always exact. The tick size (e.g. $0.01 vs $0.0001) is a
// concern for whatever layer converts external prices into ticks, not for
// this package.
type Order struct {
	ID        string
	Side      Side
	Price     int64 // price in ticks, not float64 — see type comment above
	Quantity  int64
	Timestamp time.Time
	OrderType OrderType
}
