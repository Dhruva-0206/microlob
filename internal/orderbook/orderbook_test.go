package orderbook

import (
	"errors"
	"testing"
	"time"
)

func mkOrder(id string, side Side, price, qty int64) Order {
	return Order{
		ID:        id,
		Side:      side,
		Price:     price,
		Quantity:  qty,
		Timestamp: time.Now(),
		OrderType: Limit,
	}
}

func orderPtr(o Order) *Order { return &o }

func TestPriceLevel_FIFOOrdering(t *testing.T) {
	tests := []struct {
		name     string
		orderIDs []string
		qtys     []int64
	}{
		{
			name:     "three orders same price stay in arrival order",
			orderIDs: []string{"o1", "o2", "o3"},
			qtys:     []int64{5, 10, 15},
		},
		{
			name:     "single order",
			orderIDs: []string{"solo"},
			qtys:     []int64{7},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := NewOrderBook("TEST")
			const price = int64(10000)

			var wantQty int64
			for i, id := range tt.orderIDs {
				if err := ob.AddOrder(mkOrder(id, Buy, price, tt.qtys[i])); err != nil {
					t.Fatalf("AddOrder(%s): %v", id, err)
				}
				wantQty += tt.qtys[i]
			}

			level, ok := ob.BidLevel(price)
			if !ok {
				t.Fatalf("expected a bid level at price %d", price)
			}

			got := level.Orders()
			if len(got) != len(tt.orderIDs) {
				t.Fatalf("level has %d orders, want %d", len(got), len(tt.orderIDs))
			}
			for i, o := range got {
				if o.ID != tt.orderIDs[i] {
					t.Errorf("position %d: got order %s, want %s (FIFO order violated)", i, o.ID, tt.orderIDs[i])
				}
			}

			if got := level.TotalQuantity(); got != wantQty {
				t.Errorf("TotalQuantity() = %d, want %d", got, wantQty)
			}
		})
	}
}

func TestOrderBook_BestBidAsk(t *testing.T) {
	type step struct {
		add    *Order
		cancel string
	}

	tests := []struct {
		name          string
		steps         []step
		wantBidPrice  int64
		wantBidExists bool
		wantAskPrice  int64
		wantAskExists bool
	}{
		{
			name: "empty book has no best bid or ask",
		},
		{
			name: "single bid and single ask",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 100, 10))},
				{add: orderPtr(mkOrder("a1", Sell, 200, 10))},
			},
			wantBidPrice: 100, wantBidExists: true,
			wantAskPrice: 200, wantAskExists: true,
		},
		{
			name: "a higher bid becomes the new best bid",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 100, 10))},
				{add: orderPtr(mkOrder("b2", Buy, 105, 10))},
			},
			wantBidPrice: 105, wantBidExists: true,
		},
		{
			name: "a lower ask becomes the new best ask",
			steps: []step{
				{add: orderPtr(mkOrder("a1", Sell, 200, 10))},
				{add: orderPtr(mkOrder("a2", Sell, 195, 10))},
			},
			wantAskPrice: 195, wantAskExists: true,
		},
		{
			name: "a lower bid does NOT become the best bid",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 105, 10))},
				{add: orderPtr(mkOrder("b2", Buy, 100, 10))},
			},
			wantBidPrice: 105, wantBidExists: true,
		},
		{
			name: "canceling the only order at the best bid falls back to next-best price",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 100, 10))},
				{add: orderPtr(mkOrder("b2", Buy, 105, 10))},
				{cancel: "b2"},
			},
			wantBidPrice: 100, wantBidExists: true,
		},
		{
			name: "canceling the last remaining bid clears best bid entirely",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 100, 10))},
				{cancel: "b1"},
			},
		},
		{
			name: "canceling the only order at the best ask falls back to next-best price",
			steps: []step{
				{add: orderPtr(mkOrder("a1", Sell, 200, 10))},
				{add: orderPtr(mkOrder("a2", Sell, 195, 10))},
				{cancel: "a2"},
			},
			wantAskPrice: 200, wantAskExists: true,
		},
		{
			name: "canceling one order at the best level, when others remain, keeps the level",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 100, 10))},
				{add: orderPtr(mkOrder("b2", Buy, 100, 5))},
				{cancel: "b1"},
			},
			wantBidPrice: 100, wantBidExists: true,
		},
		{
			name: "bids and asks are tracked independently",
			steps: []step{
				{add: orderPtr(mkOrder("b1", Buy, 100, 10))},
				{add: orderPtr(mkOrder("b2", Buy, 110, 10))},
				{add: orderPtr(mkOrder("a1", Sell, 200, 10))},
				{add: orderPtr(mkOrder("a2", Sell, 190, 10))},
				{cancel: "b2"},
				{cancel: "a2"},
			},
			wantBidPrice: 100, wantBidExists: true,
			wantAskPrice: 200, wantAskExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := NewOrderBook("TEST")

			for _, s := range tt.steps {
				switch {
				case s.add != nil:
					if err := ob.AddOrder(*s.add); err != nil {
						t.Fatalf("AddOrder(%s): %v", s.add.ID, err)
					}
				case s.cancel != "":
					if err := ob.CancelOrder(s.cancel); err != nil {
						t.Fatalf("CancelOrder(%s): %v", s.cancel, err)
					}
				}
			}

			gotBidPrice, gotBidExists := ob.BestBid()
			if gotBidExists != tt.wantBidExists || (gotBidExists && gotBidPrice != tt.wantBidPrice) {
				t.Errorf("BestBid() = (%d, %v), want (%d, %v)", gotBidPrice, gotBidExists, tt.wantBidPrice, tt.wantBidExists)
			}

			gotAskPrice, gotAskExists := ob.BestAsk()
			if gotAskExists != tt.wantAskExists || (gotAskExists && gotAskPrice != tt.wantAskPrice) {
				t.Errorf("BestAsk() = (%d, %v), want (%d, %v)", gotAskPrice, gotAskExists, tt.wantAskPrice, tt.wantAskExists)
			}
		})
	}
}

func TestOrderBook_CancelOrder_RemovesOrderCompletely(t *testing.T) {
	ob := NewOrderBook("TEST")
	if err := ob.AddOrder(mkOrder("b1", Buy, 100, 10)); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}
	if err := ob.AddOrder(mkOrder("b2", Buy, 100, 5)); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}

	if err := ob.CancelOrder("b1"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	level, ok := ob.BidLevel(100)
	if !ok {
		t.Fatalf("expected level at 100 to still exist (b2 remains)")
	}
	for _, o := range level.Orders() {
		if o.ID == "b1" {
			t.Errorf("canceled order b1 is still present in the price level")
		}
	}
	if got, want := level.TotalQuantity(), int64(5); got != want {
		t.Errorf("TotalQuantity() = %d, want %d", got, want)
	}

	// Canceling again must fail: the order is gone.
	if err := ob.CancelOrder("b1"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("CancelOrder(already-canceled) = %v, want ErrOrderNotFound", err)
	}
}

func TestOrderBook_CancelOrder_EmptyLevelIsRemoved(t *testing.T) {
	ob := NewOrderBook("TEST")
	if err := ob.AddOrder(mkOrder("b1", Buy, 100, 10)); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}
	if err := ob.CancelOrder("b1"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if _, ok := ob.BidLevel(100); ok {
		t.Errorf("expected price level at 100 to be removed once its last order was canceled")
	}
}

func TestOrderBook_AddOrder_Validation(t *testing.T) {
	tests := []struct {
		name    string
		order   Order
		wantErr error
	}{
		{
			name:    "empty ID is rejected",
			order:   mkOrder("", Buy, 100, 10),
			wantErr: ErrInvalidOrder,
		},
		{
			name:    "zero quantity is rejected",
			order:   mkOrder("o1", Buy, 100, 0),
			wantErr: ErrInvalidOrder,
		},
		{
			name:    "negative price is rejected",
			order:   mkOrder("o1", Buy, -1, 10),
			wantErr: ErrInvalidOrder,
		},
		{
			name: "market orders are rejected (no matching engine yet)",
			order: Order{
				ID: "o1", Side: Buy, Price: 100, Quantity: 10,
				Timestamp: time.Now(), OrderType: Market,
			},
			wantErr: ErrMarketOrderNotSupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := NewOrderBook("TEST")
			err := ob.AddOrder(tt.order)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("AddOrder() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestOrderBook_AddOrder_RejectsDuplicateID(t *testing.T) {
	ob := NewOrderBook("TEST")
	if err := ob.AddOrder(mkOrder("dup", Buy, 100, 10)); err != nil {
		t.Fatalf("AddOrder: %v", err)
	}
	err := ob.AddOrder(mkOrder("dup", Sell, 200, 5))
	if !errors.Is(err, ErrDuplicateOrder) {
		t.Errorf("AddOrder(duplicate id) error = %v, want ErrDuplicateOrder", err)
	}
}
