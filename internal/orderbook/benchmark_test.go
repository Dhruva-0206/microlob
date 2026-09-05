package orderbook

import (
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

// Orders are priced in a band around a mid-price so a realistic mix of
// them cross immediately (generating a Trade) and rest on the book,
// instead of every order piling up on one side or every order matching.
const (
	benchMidPrice    = int64(10_000)
	benchPriceSpread = int64(50)
	benchMinQty      = int64(1)
	benchQtySpread   = int64(100)
)

func benchOrder(rng *rand.Rand, id string) Order {
	side := Buy
	if rng.Intn(2) == 1 {
		side = Sell
	}
	price := benchMidPrice + rng.Int63n(2*benchPriceSpread+1) - benchPriceSpread
	qty := benchMinQty + rng.Int63n(benchQtySpread)
	return Order{
		ID:        id,
		Side:      side,
		Price:     price,
		Quantity:  qty,
		Timestamp: time.Now(),
		OrderType: Limit,
	}
}

// BenchmarkSubmitOrder measures sequential SubmitOrder throughput/latency
// through a single Engine: one goroutine, one order at a time — the
// simplest load shape and a baseline for the parallel variant below.
//
// Orders are generated up front, before ResetTimer, so the measured loop
// contains nothing but the SubmitOrder call itself: no order construction,
// no ID formatting, no engine startup.
func BenchmarkSubmitOrder(b *testing.B) {
	engine := NewEngine("BENCH")
	engine.Start()
	defer engine.Stop()

	orders := make([]Order, b.N)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < b.N; i++ {
		orders[i] = benchOrder(rng, fmt.Sprintf("bench-%d", i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.SubmitOrder(orders[i]); err != nil {
			b.Fatalf("SubmitOrder: %v", err)
		}
	}
}

// BenchmarkSubmitOrder_Parallel measures throughput under concurrent
// submitters — the load shape Engine is actually built for, closer to a
// real simulation where many independent agent goroutines all call
// SubmitOrder against the same Engine at once. Engine's single-writer
// goroutine still processes every order one at a time internally; this
// benchmark is what shows whether the channel handoff itself holds up
// under concurrent callers.
//
// As with the sequential benchmark, every order is pre-generated before
// ResetTimer; each parallel worker claims one via an atomic counter so the
// timed region is just the SubmitOrder call.
func BenchmarkSubmitOrder_Parallel(b *testing.B) {
	engine := NewEngine("BENCH_PARALLEL")
	engine.Start()
	defer engine.Stop()

	orders := make([]Order, b.N)
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < b.N; i++ {
		orders[i] = benchOrder(rng, fmt.Sprintf("bench-p-%d", i))
	}

	var next int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddInt64(&next, 1) - 1
			if _, err := engine.SubmitOrder(orders[i]); err != nil {
				b.Fatalf("SubmitOrder: %v", err)
			}
		}
	})
}
