package orderbook

import (
	"container/list"
	"errors"
	"fmt"
	"sort"
	"time"
)

var (
	ErrInvalidOrder            = errors.New("orderbook: invalid order")
	ErrDuplicateOrder          = errors.New("orderbook: duplicate order id")
	ErrOrderNotFound           = errors.New("orderbook: order not found")
	ErrMarketOrderNotSupported = errors.New("orderbook: market orders can't rest on the book (no matching engine yet)")
)

// orderLocation is what OrderBook's order index stores per order ID: enough
// to jump straight to the order's PriceLevel and its element within that
// level's queue, without scanning anything. This is what makes CancelOrder
// O(1) (plus the O(log n)/O(n) cost of removing a price from the sorted
// slice, only when a level empties out — see removePrice).
type orderLocation struct {
	side  Side
	price int64
	elem  *list.Element
}

// OrderBook holds the resting orders for a single symbol.
//
// Best bid/ask tracking: alongside the price->*PriceLevel maps, each side
// keeps a slice of its price levels sorted in ascending order
// (bidPrices/askPrices). This makes BestBid/BestAsk O(1): the best bid is
// the highest bid price, i.e. the last element of bidPrices; the best ask is
// the lowest ask price, i.e. the first element of askPrices.
//
// Why a sorted slice instead of just scanning the map, or a heap?
//   - A map has no ordering at all, so finding the best price would mean
//     scanning every key on every call — O(n) just to answer "what's the
//     best bid," which is the single most frequently-asked question in an
//     order book.
//   - A heap gives O(log n) insert and O(1) peek at the top, which sounds
//     ideal, but a level isn't removed only when its price stops being the
//     best — it's removed whenever it empties out, which can happen to any
//     price level, at any position in the heap, when its last order is
//     cancelled or filled. Removing an arbitrary element from a
//     container/heap still costs O(log n) *if* you already know its index,
//     but knowing that index requires a separate price->index map kept in
//     sync on every sift-up/sift-down — real, but it's extra bookkeeping for
//     a marginal win at this stage.
//   - A sorted slice makes the read path (BestBid/BestAsk) trivially O(1)
//     with zero extra indices, and insert/remove is a binary search
//     (sort.Search, O(log n)) to find the position plus an O(n) shift to
//     keep the slice contiguous. For a learning-stage book this is simple,
//     correct, and fast enough; if profiling later shows the O(n) shift
//     matters (very deep books with high price-level churn), the natural
//     upgrade is a heap plus a price->index map, or an order-statistics
//     tree.
type OrderBook struct {
	Symbol string

	bids map[int64]*PriceLevel
	asks map[int64]*PriceLevel

	bidPrices []int64 // ascending; best bid = highest = bidPrices[len-1]
	askPrices []int64 // ascending; best ask = lowest  = askPrices[0]

	orderIndex map[string]*orderLocation // orderID -> location, for O(1) cancel

	tradeSeq int64 // monotonic counter backing nextTradeID
}

// NewOrderBook creates an empty order book for the given symbol.
func NewOrderBook(symbol string) *OrderBook {
	return &OrderBook{
		Symbol:     symbol,
		bids:       make(map[int64]*PriceLevel),
		asks:       make(map[int64]*PriceLevel),
		orderIndex: make(map[string]*orderLocation),
	}
}

// validateOrder checks the fields every order must satisfy regardless of
// how it's submitted. Limit orders need a positive price to rest at; Market
// orders don't (they trade at whatever price the book offers), so Price is
// only checked when it's actually going to be used.
func validateOrder(order Order) error {
	if order.ID == "" {
		return fmt.Errorf("%w: empty order id", ErrInvalidOrder)
	}
	if order.Quantity <= 0 {
		return fmt.Errorf("%w: quantity must be positive, got %d", ErrInvalidOrder, order.Quantity)
	}
	if order.Side != Buy && order.Side != Sell {
		return fmt.Errorf("%w: unknown side %v", ErrInvalidOrder, order.Side)
	}
	switch order.OrderType {
	case Limit:
		if order.Price <= 0 {
			return fmt.Errorf("%w: limit order price must be positive, got %d", ErrInvalidOrder, order.Price)
		}
	case Market:
		// Price is ignored: a market order crosses at whatever the best
		// available resting price is.
	default:
		return fmt.Errorf("%w: unknown order type %v", ErrInvalidOrder, order.OrderType)
	}
	return nil
}

// SubmitOrder is the entrypoint for new orders: it matches the incoming
// order against the resting book first, and only rests whatever quantity is
// left over (Limit orders only — Market orders never rest, see below). This
// is what callers outside the package should use; AddOrder is kept around as
// the internal helper SubmitOrder uses to rest that leftover quantity (and
// as a way for tests to seed resting book state directly, without needing a
// counterparty order to make it "stick").
//
// Matching walks the opposite side's best price level in FIFO order — the
// order that arrived first at that price trades first — and keeps moving to
// the next-best price level as long as the incoming order still has
// quantity left and its price still crosses. Each match produces a Trade at
// the resting order's price (price-time priority, see Trade's doc comment).
// A fully-filled resting order is removed from the book; a partially-filled
// one keeps resting with its Quantity reduced in place.
func (ob *OrderBook) SubmitOrder(order Order) ([]Trade, error) {
	if err := validateOrder(order); err != nil {
		return nil, err
	}
	if _, exists := ob.orderIndex[order.ID]; exists {
		return nil, fmt.Errorf("%w: %q", ErrDuplicateOrder, order.ID)
	}

	trades := make([]Trade, 0)
	levels, prices := ob.oppositeState(order.Side)

	for order.Quantity > 0 {
		bestPrice, ok := ob.bestOppositePrice(order.Side)
		if !ok || !canCross(order, bestPrice) {
			break
		}

		level := levels[bestPrice]
		elem := level.FrontElement()
		for elem != nil && order.Quantity > 0 {
			next := elem.Next() // Remove below clears elem's own links
			resting := elem.Value.(*Order)

			tradeQty := order.Quantity
			if resting.Quantity < tradeQty {
				tradeQty = resting.Quantity
			}

			trade := Trade{
				ID:        ob.nextTradeID(),
				Price:     resting.Price,
				Quantity:  tradeQty,
				Timestamp: time.Now(),
			}
			if order.Side == Buy {
				trade.BuyOrderID, trade.SellOrderID = order.ID, resting.ID
			} else {
				trade.BuyOrderID, trade.SellOrderID = resting.ID, order.ID
			}
			trades = append(trades, trade)

			order.Quantity -= tradeQty
			resting.Quantity -= tradeQty
			if resting.Quantity == 0 {
				level.Remove(elem)
				delete(ob.orderIndex, resting.ID)
			}

			elem = next
		}

		if level.Len() == 0 {
			delete(levels, bestPrice)
			*prices = removePrice(*prices, bestPrice)
		}
	}

	if order.Quantity > 0 && order.OrderType == Limit {
		if err := ob.AddOrder(order); err != nil {
			return trades, err
		}
	}
	// A Market order with leftover quantity is dropped here: market orders
	// never wait around on the book for a future counterparty.

	return trades, nil
}

// nextTradeID generates a unique, monotonically increasing ID for each
// trade this book produces.
func (ob *OrderBook) nextTradeID() string {
	ob.tradeSeq++
	return fmt.Sprintf("%s-TRD-%d", ob.Symbol, ob.tradeSeq)
}

// canCross reports whether incoming (at its own price, ignored for Market
// orders) is willing to trade against a resting order at restingPrice.
func canCross(incoming Order, restingPrice int64) bool {
	if incoming.OrderType == Market {
		return true
	}
	if incoming.Side == Buy {
		return incoming.Price >= restingPrice
	}
	return incoming.Price <= restingPrice
}

// bestOppositePrice returns the best resting price on the side opposite the
// given side: the best ask for a Buy order to match against, the best bid
// for a Sell order.
func (ob *OrderBook) bestOppositePrice(side Side) (int64, bool) {
	if side == Buy {
		return ob.BestAsk()
	}
	return ob.BestBid()
}

// oppositeState mirrors sideState but for the resting side an incoming
// order matches against rather than the side it would itself join.
func (ob *OrderBook) oppositeState(side Side) (levels map[int64]*PriceLevel, prices *[]int64) {
	if side == Buy {
		return ob.asks, &ob.askPrices
	}
	return ob.bids, &ob.bidPrices
}

// AddOrder inserts an order into the book without any matching — it always
// rests at its specified price, as if the book had no counterparty for it.
// It's an internal helper: SubmitOrder calls it only to rest an incoming
// Limit order's leftover quantity after matching. Market orders are
// rejected outright since they have no price to rest at.
func (ob *OrderBook) AddOrder(order Order) error {
	if err := validateOrder(order); err != nil {
		return err
	}
	if order.OrderType == Market {
		return ErrMarketOrderNotSupported
	}
	if _, exists := ob.orderIndex[order.ID]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateOrder, order.ID)
	}

	levels, prices := ob.sideState(order.Side)

	level, ok := levels[order.Price]
	if !ok {
		level = newPriceLevel(order.Price)
		levels[order.Price] = level
		*prices = insertPrice(*prices, order.Price)
	}

	stored := order
	elem := level.Push(&stored)

	ob.orderIndex[order.ID] = &orderLocation{
		side:  order.Side,
		price: order.Price,
		elem:  elem,
	}
	return nil
}

// CancelOrder removes a resting order by ID from wherever it is on the book.
// If it was the last order at its price level, the level and its price are
// removed too, so BestBid/BestAsk stay accurate. This works the same
// whether the order started resting via AddOrder directly or as leftover
// quantity rested by SubmitOrder — both paths register it in orderIndex the
// same way, so cancellation doesn't need to know which one put it there.
func (ob *OrderBook) CancelOrder(orderID string) error {
	loc, ok := ob.orderIndex[orderID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrOrderNotFound, orderID)
	}

	levels, prices := ob.sideState(loc.side)
	level := levels[loc.price]
	level.Remove(loc.elem)
	delete(ob.orderIndex, orderID)

	if level.Len() == 0 {
		delete(levels, loc.price)
		*prices = removePrice(*prices, loc.price)
	}
	return nil
}

// BestBid returns the highest price with resting buy orders.
func (ob *OrderBook) BestBid() (price int64, exists bool) {
	if len(ob.bidPrices) == 0 {
		return 0, false
	}
	return ob.bidPrices[len(ob.bidPrices)-1], true
}

// BestAsk returns the lowest price with resting sell orders.
func (ob *OrderBook) BestAsk() (price int64, exists bool) {
	if len(ob.askPrices) == 0 {
		return 0, false
	}
	return ob.askPrices[0], true
}

// BidLevel returns the bid-side price level at price, if one exists.
func (ob *OrderBook) BidLevel(price int64) (*PriceLevel, bool) {
	pl, ok := ob.bids[price]
	return pl, ok
}

// AskLevel returns the ask-side price level at price, if one exists.
func (ob *OrderBook) AskLevel(price int64) (*PriceLevel, bool) {
	pl, ok := ob.asks[price]
	return pl, ok
}

// sideState returns the level map and a pointer to the sorted price slice
// for the given side, so AddOrder/CancelOrder don't need to duplicate the
// Buy/Sell switch.
func (ob *OrderBook) sideState(side Side) (levels map[int64]*PriceLevel, prices *[]int64) {
	if side == Buy {
		return ob.bids, &ob.bidPrices
	}
	return ob.asks, &ob.askPrices
}

// insertPrice inserts price into an ascending sorted slice, keeping it
// sorted, via binary search for the insertion point.
func insertPrice(prices []int64, price int64) []int64 {
	idx := sort.Search(len(prices), func(i int) bool { return prices[i] >= price })
	prices = append(prices, 0)
	copy(prices[idx+1:], prices[idx:])
	prices[idx] = price
	return prices
}

// removePrice removes price from an ascending sorted slice, if present.
func removePrice(prices []int64, price int64) []int64 {
	idx := sort.Search(len(prices), func(i int) bool { return prices[i] >= price })
	if idx < len(prices) && prices[idx] == price {
		prices = append(prices[:idx], prices[idx+1:]...)
	}
	return prices
}
