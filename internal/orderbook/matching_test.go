package orderbook

import (
	"errors"
	"testing"
	"time"
)

func mkMarketOrder(id string, side Side, qty int64) Order {
	return Order{
		ID:        id,
		Side:      side,
		Quantity:  qty,
		Timestamp: time.Now(),
		OrderType: Market,
	}
}

// wantTrade is the subset of Trade fields worth asserting on in tests: ID is
// generated internally and Timestamp is wall-clock, so both are checked only
// for "is it populated," never for an exact value.
type wantTrade struct {
	buyID, sellID string
	price, qty    int64
}

func requireTrades(t *testing.T, got []Trade, want []wantTrade) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d trades %+v, want %d trades %+v", len(got), got, len(want), want)
	}
	for i, tr := range got {
		w := want[i]
		if tr.BuyOrderID != w.buyID || tr.SellOrderID != w.sellID || tr.Price != w.price || tr.Quantity != w.qty {
			t.Errorf("trade[%d] = {buy:%s sell:%s price:%d qty:%d}, want {buy:%s sell:%s price:%d qty:%d}",
				i, tr.BuyOrderID, tr.SellOrderID, tr.Price, tr.Quantity, w.buyID, w.sellID, w.price, w.qty)
		}
		if tr.ID == "" {
			t.Errorf("trade[%d] has an empty ID", i)
		}
		if tr.Timestamp.IsZero() {
			t.Errorf("trade[%d] has a zero Timestamp", i)
		}
	}
}

func TestOrderBook_SubmitOrder_Matching(t *testing.T) {
	tests := []struct {
		name          string
		setup         []Order // resting orders added first; must not cross each other
		incoming      Order   // the order under test
		wantTrades    []wantTrade
		wantBidPrice  int64
		wantBidExists bool
		wantAskPrice  int64
		wantAskExists bool
		check         func(t *testing.T, ob *OrderBook) // optional extra assertions
	}{
		{
			name:       "fully fills against one resting order",
			setup:      []Order{mkOrder("s1", Sell, 100, 10)},
			incoming:   mkOrder("b1", Buy, 100, 10),
			wantTrades: []wantTrade{{"b1", "s1", 100, 10}},
			// Both sides empty: the resting sell was fully consumed and the
			// incoming buy had nothing left over to rest.
		},
		{
			name:          "partially fills against one resting order; leftover keeps resting",
			setup:         []Order{mkOrder("s1", Sell, 100, 10)},
			incoming:      mkOrder("b1", Buy, 100, 4),
			wantTrades:    []wantTrade{{"b1", "s1", 100, 4}},
			wantAskPrice:  100,
			wantAskExists: true,
			check: func(t *testing.T, ob *OrderBook) {
				level, ok := ob.AskLevel(100)
				if !ok {
					t.Fatalf("expected ask level at 100 to still exist")
				}
				if got, want := level.TotalQuantity(), int64(6); got != want {
					t.Errorf("resting s1 quantity = %d, want %d", got, want)
				}
			},
		},
		{
			name: "consumes multiple resting orders at increasingly worse prices",
			setup: []Order{
				mkOrder("s1", Sell, 100, 5),
				mkOrder("s2", Sell, 101, 5),
				mkOrder("s3", Sell, 102, 5),
			},
			incoming: mkOrder("b1", Buy, 102, 12),
			wantTrades: []wantTrade{
				{"b1", "s1", 100, 5},
				{"b1", "s2", 101, 5},
				{"b1", "s3", 102, 2},
			},
			wantAskPrice:  102,
			wantAskExists: true,
			check: func(t *testing.T, ob *OrderBook) {
				level, ok := ob.AskLevel(102)
				if !ok {
					t.Fatalf("expected ask level at 102 to still exist")
				}
				if got, want := level.TotalQuantity(), int64(3); got != want {
					t.Errorf("resting s3 quantity = %d, want %d", got, want)
				}
				if _, ok := ob.AskLevel(100); ok {
					t.Errorf("ask level at 100 should have been removed once s1 was fully filled")
				}
				if _, ok := ob.AskLevel(101); ok {
					t.Errorf("ask level at 101 should have been removed once s2 was fully filled")
				}
			},
		},
		{
			name: "time priority: the earlier resting order at a price trades first",
			setup: []Order{
				mkOrder("s1", Sell, 100, 5),
				mkOrder("s2", Sell, 100, 5),
			},
			incoming: mkOrder("b1", Buy, 100, 7),
			wantTrades: []wantTrade{
				{"b1", "s1", 100, 5},
				{"b1", "s2", 100, 2},
			},
			wantAskPrice:  100,
			wantAskExists: true,
			check: func(t *testing.T, ob *OrderBook) {
				level, ok := ob.AskLevel(100)
				if !ok {
					t.Fatalf("expected ask level at 100 to still exist")
				}
				orders := level.Orders()
				if len(orders) != 1 || orders[0].ID != "s2" {
					t.Fatalf("expected only s2 left resting at 100, got %+v", orders)
				}
				if got, want := orders[0].Quantity, int64(3); got != want {
					t.Errorf("resting s2 quantity = %d, want %d", got, want)
				}
			},
		},
		{
			name:          "no cross: incoming order does not match and just rests",
			setup:         []Order{mkOrder("s1", Sell, 100, 5)},
			incoming:      mkOrder("b1", Buy, 95, 10),
			wantTrades:    []wantTrade{},
			wantBidPrice:  95,
			wantBidExists: true,
			wantAskPrice:  100,
			wantAskExists: true,
			check: func(t *testing.T, ob *OrderBook) {
				level, ok := ob.BidLevel(95)
				if !ok {
					t.Fatalf("expected incoming order to rest at 95")
				}
				orders := level.Orders()
				if len(orders) != 1 || orders[0].ID != "b1" || orders[0].Quantity != 10 {
					t.Errorf("resting level at 95 = %+v, want [{b1 qty:10}]", orders)
				}
			},
		},
		{
			name:       "market order fully fills",
			setup:      []Order{mkOrder("s1", Sell, 100, 10)},
			incoming:   mkMarketOrder("m1", Buy, 10),
			wantTrades: []wantTrade{{"m1", "s1", 100, 10}},
		},
		{
			name:       "market order partially fills and does NOT rest the remainder",
			setup:      []Order{mkOrder("s1", Sell, 100, 4)},
			incoming:   mkMarketOrder("m1", Buy, 10),
			wantTrades: []wantTrade{{"m1", "s1", 100, 4}},
			check: func(t *testing.T, ob *OrderBook) {
				if err := ob.CancelOrder("m1"); !errors.Is(err, ErrOrderNotFound) {
					t.Errorf("CancelOrder(m1) = %v, want ErrOrderNotFound (market leftover must not rest)", err)
				}
			},
		},
		{
			name: "best bid and best ask update independently after a trade",
			setup: []Order{
				mkOrder("b1", Buy, 99, 5),
				mkOrder("b2", Buy, 100, 5),
				mkOrder("s1", Sell, 101, 5),
				mkOrder("s2", Sell, 102, 5),
			},
			incoming:      mkOrder("s3", Sell, 99, 5), // crosses the best bid (100)
			wantTrades:    []wantTrade{{"b2", "s3", 100, 5}},
			wantBidPrice:  99,
			wantBidExists: true,
			wantAskPrice:  101,
			wantAskExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := NewOrderBook("TEST")

			for _, o := range tt.setup {
				trades, err := ob.SubmitOrder(o)
				if err != nil {
					t.Fatalf("setup SubmitOrder(%s): %v", o.ID, err)
				}
				if len(trades) != 0 {
					t.Fatalf("setup order %s unexpectedly matched: %+v", o.ID, trades)
				}
			}

			gotTrades, err := ob.SubmitOrder(tt.incoming)
			if err != nil {
				t.Fatalf("SubmitOrder(%s): %v", tt.incoming.ID, err)
			}
			requireTrades(t, gotTrades, tt.wantTrades)

			gotBidPrice, gotBidExists := ob.BestBid()
			if gotBidExists != tt.wantBidExists || (gotBidExists && gotBidPrice != tt.wantBidPrice) {
				t.Errorf("BestBid() = (%d, %v), want (%d, %v)", gotBidPrice, gotBidExists, tt.wantBidPrice, tt.wantBidExists)
			}
			gotAskPrice, gotAskExists := ob.BestAsk()
			if gotAskExists != tt.wantAskExists || (gotAskExists && gotAskPrice != tt.wantAskPrice) {
				t.Errorf("BestAsk() = (%d, %v), want (%d, %v)", gotAskPrice, gotAskExists, tt.wantAskPrice, tt.wantAskExists)
			}

			if tt.check != nil {
				tt.check(t, ob)
			}
		})
	}
}

func TestOrderBook_SubmitOrder_TradeIDsAreUnique(t *testing.T) {
	ob := NewOrderBook("TEST")
	if _, err := ob.SubmitOrder(mkOrder("s1", Sell, 100, 5)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if _, err := ob.SubmitOrder(mkOrder("s2", Sell, 100, 5)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	trades, err := ob.SubmitOrder(mkOrder("b1", Buy, 100, 10))
	if err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2", len(trades))
	}
	if trades[0].ID == trades[1].ID {
		t.Errorf("trade IDs must be unique, both were %q", trades[0].ID)
	}
}

func TestOrderBook_SubmitOrder_RejectsDuplicateID(t *testing.T) {
	ob := NewOrderBook("TEST")
	if _, err := ob.SubmitOrder(mkOrder("dup", Buy, 100, 10)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	_, err := ob.SubmitOrder(mkOrder("dup", Sell, 200, 5))
	if !errors.Is(err, ErrDuplicateOrder) {
		t.Errorf("SubmitOrder(duplicate id) error = %v, want ErrDuplicateOrder", err)
	}
}
