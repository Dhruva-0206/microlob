package orderbook

import "container/list"

// PriceLevel holds every resting order at a single price, in strict
// time-price priority: the order that arrived first is filled first.
//
// It's backed by container/list, a doubly-linked list, rather than a slice:
//   - New orders always join the back of the queue (PushBack), which is O(1).
//   - Cancellations can remove an order from anywhere in the queue — the
//     front, the back, or the middle — in O(1), because list.Remove needs
//     only the *list.Element, not its position. A slice-based queue would
//     need an O(n) shift to remove an arbitrary element while preserving the
//     order of the rest.
//
// The tradeoff is that OrderBook must hang on to the *list.Element returned
// by Push (it does, in its order index) so a future cancel doesn't need to
// scan the level looking for the right order.
type PriceLevel struct {
	Price  int64
	orders *list.List
}

func newPriceLevel(price int64) *PriceLevel {
	return &PriceLevel{
		Price:  price,
		orders: list.New(),
	}
}

// Push adds an order to the back of the FIFO queue and returns the element
// handle, which callers must retain to support O(1) removal later.
func (pl *PriceLevel) Push(o *Order) *list.Element {
	return pl.orders.PushBack(o)
}

// Remove takes an order out of the queue, given the element handle returned
// by Push. This is how cancellation removes an order "from anywhere" in the
// queue without scanning it.
func (pl *PriceLevel) Remove(e *list.Element) {
	pl.orders.Remove(e)
}

// FrontElement returns the element at the front of the FIFO queue — the
// order with priority to trade next — or nil if the level is empty. The
// matching engine walks the queue from here, mutating each order's
// Quantity in place (the list stores *Order, so the element's value is a
// pointer) and calling Remove once an order is fully filled.
func (pl *PriceLevel) FrontElement() *list.Element {
	return pl.orders.Front()
}

// Len reports how many orders are resting at this level.
func (pl *PriceLevel) Len() int {
	return pl.orders.Len()
}

// TotalQuantity sums the quantity of every order resting at this level.
func (pl *PriceLevel) TotalQuantity() int64 {
	var total int64
	for e := pl.orders.Front(); e != nil; e = e.Next() {
		total += e.Value.(*Order).Quantity
	}
	return total
}

// Orders returns the resting orders in FIFO priority order (oldest first).
// It allocates a fresh slice, so it's meant for inspection/testing, not a
// hot path.
func (pl *PriceLevel) Orders() []*Order {
	orders := make([]*Order, 0, pl.orders.Len())
	for e := pl.orders.Front(); e != nil; e = e.Next() {
		orders = append(orders, e.Value.(*Order))
	}
	return orders
}
