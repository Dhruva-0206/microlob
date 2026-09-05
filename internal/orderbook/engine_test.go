package orderbook

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestEngine_SubmitCancelBestBidAsk(t *testing.T) {
	e := NewEngine("TEST")
	e.Start()
	defer e.Stop()

	if _, err := e.SubmitOrder(mkOrder("s1", Sell, 100, 10)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if price, exists := e.BestAsk(); !exists || price != 100 {
		t.Fatalf("BestAsk() = (%d, %v), want (100, true)", price, exists)
	}
	if _, exists := e.BestBid(); exists {
		t.Fatalf("BestBid() exists, want none")
	}

	trades, err := e.SubmitOrder(mkOrder("b1", Buy, 100, 4))
	if err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if len(trades) != 1 || trades[0].Quantity != 4 || trades[0].Price != 100 {
		t.Fatalf("trades = %+v, want one trade at price 100 qty 4", trades)
	}
	if price, exists := e.BestAsk(); !exists || price != 100 {
		t.Fatalf("BestAsk() after partial fill = (%d, %v), want (100, true)", price, exists)
	}

	if _, err := e.SubmitOrder(mkOrder("b2", Buy, 95, 5)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	if price, exists := e.BestBid(); !exists || price != 95 {
		t.Fatalf("BestBid() = (%d, %v), want (95, true)", price, exists)
	}

	if err := e.CancelOrder("b2"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if _, exists := e.BestBid(); exists {
		t.Fatalf("BestBid() exists after canceling the only bid, want none")
	}

	if err := e.CancelOrder("b2"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("CancelOrder(already-canceled) = %v, want ErrOrderNotFound", err)
	}
}

// callWithTimeout runs fn in its own goroutine and fails the test if it
// doesn't return within the given timeout — used below to prove a stopped
// Engine's methods return promptly instead of hanging forever waiting on a
// channel nobody will ever service again.
func callWithTimeout(t *testing.T, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("call did not return in time — Engine appears to be hanging after Stop")
	}
}

func TestEngine_StopIsGraceful(t *testing.T) {
	e := NewEngine("TEST")
	e.Start()
	if _, err := e.SubmitOrder(mkOrder("s1", Sell, 100, 10)); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}

	e.Stop()

	callWithTimeout(t, time.Second, func() {
		if _, err := e.SubmitOrder(mkOrder("s2", Sell, 100, 10)); !errors.Is(err, ErrEngineStopped) {
			t.Errorf("SubmitOrder after Stop = %v, want ErrEngineStopped", err)
		}
	})
	callWithTimeout(t, time.Second, func() {
		if err := e.CancelOrder("s1"); !errors.Is(err, ErrEngineStopped) {
			t.Errorf("CancelOrder after Stop = %v, want ErrEngineStopped", err)
		}
	})
	callWithTimeout(t, time.Second, func() {
		if _, exists := e.BestBid(); exists {
			t.Errorf("BestBid() after Stop reported exists=true, want false")
		}
	})
	callWithTimeout(t, time.Second, func() {
		if _, exists := e.BestAsk(); exists {
			t.Errorf("BestAsk() after Stop reported exists=true, want false")
		}
	})
}

// restingQuantity sums the resting quantity across every price level on
// both sides. It reaches into OrderBook's unexported fields directly
// (this file is in package orderbook), which is only safe to do here
// because the caller has already called Engine.Stop() and it has returned —
// at that point the owning goroutine is guaranteed to have exited (Stop
// blocks on the closed `stopped` channel), so there is no concurrent writer
// left and no race in reading book state directly.
func restingQuantity(book *OrderBook) int64 {
	var total int64
	for _, level := range book.bids {
		total += level.TotalQuantity()
	}
	for _, level := range book.asks {
		total += level.TotalQuantity()
	}
	return total
}

// TestEngine_ConcurrentSubmitOrder_Stress hammers a single Engine with many
// goroutines submitting randomized orders at once, to check the
// single-writer-goroutine design actually gives the safety it promises.
//
// This test MUST be run with the race detector to mean anything:
//
//	go test -race ./...
//
// A plain `go test` run can pass even with a broken, racy Engine — it's
// -race that would catch two goroutines touching OrderBook state without
// synchronization.
func TestEngine_ConcurrentSubmitOrder_Stress(t *testing.T) {
	const numWorkers = 100
	const minPrice, priceSpread = int64(95), 11 // prices 95..105 inclusive
	const minQty, qtySpread = int64(1), 20      // quantities 1..20 inclusive

	e := NewEngine("STRESS")
	e.Start()

	type workerResult struct {
		order  Order
		trades []Trade
		err    error
	}
	results := make(chan workerResult, numWorkers)

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each worker gets its own rand source (seeded from its index)
			// so generating a random order needs no shared, synchronized
			// state — keeping this test's own scaffolding race-free is
			// what lets a -race failure be attributed to Engine, not to
			// the test.
			rng := rand.New(rand.NewSource(int64(i) + 1))
			side := Buy
			if rng.Intn(2) == 1 {
				side = Sell
			}
			order := Order{
				ID:        fmt.Sprintf("stress-%d", i),
				Side:      side,
				Price:     minPrice + int64(rng.Intn(priceSpread)),
				Quantity:  minQty + int64(rng.Intn(qtySpread)),
				Timestamp: time.Now(),
				OrderType: Limit,
			}
			trades, err := e.SubmitOrder(order)
			results <- workerResult{order: order, trades: trades, err: err}
		}(i)
	}
	wg.Wait()
	close(results)

	var totalSubmitted int64
	var allTrades []Trade
	for wr := range results {
		if wr.err != nil {
			t.Fatalf("SubmitOrder(%s) returned an unexpected error: %v", wr.order.ID, wr.err)
		}
		totalSubmitted += wr.order.Quantity
		allTrades = append(allTrades, wr.trades...)
	}

	bidPrice, bidExists := e.BestBid()
	askPrice, askExists := e.BestAsk()
	if bidExists && askExists && bidPrice >= askPrice {
		t.Errorf("book is crossed after concurrent matching: bestBid=%d >= bestAsk=%d", bidPrice, askPrice)
	}

	e.Stop()

	// Quantity conservation: every unit of quantity submitted either ends
	// up resting on the book, or was matched away in a trade. A trade of
	// quantity q removes q units from *two* orders at once (the aggressor
	// and the resting order it matched against), so trades count double
	// against the total submitted across all orders.
	var totalTraded int64
	for _, tr := range allTrades {
		if tr.Quantity <= 0 {
			t.Errorf("trade %s has non-positive quantity %d", tr.ID, tr.Quantity)
		}
		totalTraded += tr.Quantity
	}
	gotResting := restingQuantity(e.book)
	if want := totalSubmitted - 2*totalTraded; gotResting != want {
		t.Errorf("resting quantity = %d, want %d (submitted=%d, traded=%d)",
			gotResting, want, totalSubmitted, totalTraded)
	}
}
