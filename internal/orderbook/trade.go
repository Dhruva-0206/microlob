package orderbook

import "time"

// Trade records a single match between an incoming order and one resting
// order. A single SubmitOrder call can generate zero, one, or many trades if
// the incoming order sweeps through multiple resting orders (at the same
// price, or successively worse ones) before it's fully filled or the book
// runs out of liquidity.
//
// Price is always the resting order's price, not the incoming order's. This
// is price-time priority: the order that was already sitting on the book
// "owns" its price, and the aggressor pays (or receives) that price rather
// than the price it was willing to trade at. E.g. a buy limit order at 105
// crossing a resting ask at 100 trades at 100, not 105 — the buyer gets the
// better price the resting seller was already offering.
type Trade struct {
	ID          string
	BuyOrderID  string
	SellOrderID string
	Price       int64
	Quantity    int64
	Timestamp   time.Time
}
