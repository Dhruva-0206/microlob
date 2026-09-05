// Command benchmark is a standalone load generator for orderbook.Engine. It
// fires a configurable number of randomized orders at an Engine through a
// configurable number of concurrent submitter goroutines, records the
// wall-clock latency of every individual SubmitOrder call, and reports
// throughput, latency percentiles, and GC activity — a closer approximation
// of a real simulation's load than a `go test -bench` run, and one that
// reports the tail-latency detail (p95/p99/max, GC pause time) that a Go
// benchmark's single ns/op number doesn't.
//
// See CLAUDE.md's "Benchmarking" section for how to read these numbers —
// in particular, why the -workers flag increases concurrent submission
// pressure without increasing the engine's own processing parallelism.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"orderbook-engine/internal/orderbook"
)

const (
	defaultOrders  = 100_000
	defaultWorkers = 8
	warmupFraction = 0.05

	// Orders are priced in a band around a mid-price so a realistic mix
	// cross immediately (generating a trade) and rest on the book.
	midPrice    = int64(10_000)
	priceSpread = int64(50)
	minQty      = int64(1)
	qtySpread   = int64(100)
)

func randomOrder(rng *rand.Rand, workerID, localIdx int) orderbook.Order {
	side := orderbook.Buy
	if rng.Intn(2) == 1 {
		side = orderbook.Sell
	}
	price := midPrice + rng.Int63n(2*priceSpread+1) - priceSpread
	qty := minQty + rng.Int63n(qtySpread)
	return orderbook.Order{
		ID:        fmt.Sprintf("w%d-%d", workerID, localIdx),
		Side:      side,
		Price:     price,
		Quantity:  qty,
		Timestamp: time.Now(),
		OrderType: orderbook.Limit,
	}
}

// percentile returns the nearest-rank value at fraction p (0..1) of an
// ascending-sorted slice.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// estimateClockResolution measures how fine-grained time.Now() actually is
// on this machine, by taking the smallest non-zero delta seen across many
// back-to-back reads. This matters because SubmitOrder's real cost (per the
// sequential go test benchmark) is on the order of ~1us — if the clock's
// own resolution is coarser than that, most individual per-call latency
// measurements below will read as exactly zero (both reads land in the same
// tick) while a few unlucky ones that straddle a tick boundary will read as
// one or more full tick-widths, which is a quantization artifact of the
// clock, not a real bimodal latency distribution. Reporting this number
// alongside the percentiles lets a reader tell the difference instead of
// mistaking clock coarseness for actual engine behavior.
// It loops until it has actually observed enough tick transitions to trust
// (not just a fixed iteration count) — a fixed count of, say, 20k reads can
// easily complete faster than a single coarse tick, which would make the
// clock look infinitely precise (0s) rather than reveal how coarse it is.
// maxSamples is just a safety cap for a genuinely high-resolution clock,
// where nearly every read differs and the target is reached almost at once.
func estimateClockResolution() time.Duration {
	const maxSamples = 2_000_000
	const wantTransitions = 20
	minDelta := time.Duration(0)
	transitions := 0
	prev := time.Now()
	for i := 0; i < maxSamples && transitions < wantTransitions; i++ {
		now := time.Now()
		if d := now.Sub(prev); d > 0 {
			transitions++
			if minDelta == 0 || d < minDelta {
				minDelta = d
			}
		}
		prev = now
	}
	return minDelta
}

type workRange struct{ lo, hi int }

// splitRanges divides [0,total) into numWorkers contiguous, non-overlapping
// ranges (as evenly as possible) so each worker can write into its own
// slice indices with no synchronization needed between workers.
func splitRanges(total, numWorkers int) []workRange {
	ranges := make([]workRange, numWorkers)
	base, rem := total/numWorkers, total%numWorkers
	cursor := 0
	for w := 0; w < numWorkers; w++ {
		size := base
		if w < rem {
			size++
		}
		ranges[w] = workRange{lo: cursor, hi: cursor + size}
		cursor += size
	}
	return ranges
}

func main() {
	numOrders := flag.Int("orders", defaultOrders, "total number of orders to submit")
	numWorkers := flag.Int("workers", defaultWorkers, "number of concurrent submitter goroutines")
	flag.Parse()

	if *numOrders <= 0 || *numWorkers <= 0 {
		fmt.Fprintln(os.Stderr, "-orders and -workers must both be positive")
		os.Exit(1)
	}

	clockResolution := estimateClockResolution()

	engine := orderbook.NewEngine("BENCHMARK")
	engine.Start()
	defer engine.Stop()

	// Per-call latency in nanoseconds, indexed by global order sequence.
	// Pre-allocated at full size so the timed loop below never grows this
	// slice — an append-driven reallocation mid-run would itself show up
	// as latency and GC pressure, distorting exactly what we're measuring.
	latencies := make([]int64, *numOrders)
	ranges := splitRanges(*numOrders, *numWorkers)

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	start := time.Now()
	var wg sync.WaitGroup
	for w, r := range ranges {
		wg.Add(1)
		go func(workerID, lo, hi int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)*2654435761 + 1))
			for i := lo; i < hi; i++ {
				order := randomOrder(rng, workerID, i-lo)
				t0 := time.Now()
				_, err := engine.SubmitOrder(order)
				latencies[i] = time.Since(t0).Nanoseconds()
				if err != nil {
					fmt.Fprintf(os.Stderr, "SubmitOrder error (worker %d, order %d): %v\n", workerID, i, err)
				}
			}
		}(w, r.lo, r.hi)
	}
	wg.Wait()
	totalDuration := time.Since(start)

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	// Drop the first ~5% of *each worker's own* index range as warm-up
	// before computing percentiles — warm-up effects (goroutine scheduling
	// settling, allocator warm-up) are per-goroutine, so discarding only a
	// single global prefix would under-warm every worker but the first.
	// The full run still counts toward total duration/throughput above;
	// only the percentile inputs exclude warm-up.
	samples := make([]int64, 0, *numOrders)
	warmupCount := 0
	for _, r := range ranges {
		n := r.hi - r.lo
		warmup := int(float64(n) * warmupFraction)
		warmupCount += warmup
		samples = append(samples, latencies[r.lo+warmup:r.hi]...)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	p50 := percentile(samples, 0.50)
	p95 := percentile(samples, 0.95)
	p99 := percentile(samples, 0.99)
	var maxLatency int64
	if len(samples) > 0 {
		maxLatency = samples[len(samples)-1]
	}

	gcCycles := memAfter.NumGC - memBefore.NumGC
	gcPause := time.Duration(memAfter.PauseTotalNs - memBefore.PauseTotalNs)
	throughput := float64(*numOrders) / totalDuration.Seconds()

	var sb strings.Builder
	fmt.Fprintf(&sb, "microlob Engine load test — %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&sb, "================================================================\n")
	fmt.Fprintf(&sb, "Total orders submitted:    %d\n", *numOrders)
	fmt.Fprintf(&sb, "Concurrent workers:        %d\n", *numWorkers)
	fmt.Fprintf(&sb, "Warm-up discarded:         %d orders (~%.0f%% of each worker's range, excluded from percentiles only)\n", warmupCount, warmupFraction*100)
	fmt.Fprintf(&sb, "\n")
	fmt.Fprintf(&sb, "Total wall-clock duration: %s\n", totalDuration)
	fmt.Fprintf(&sb, "Throughput:                %.0f orders/sec\n", throughput)
	fmt.Fprintf(&sb, "\n")
	fmt.Fprintf(&sb, "Observed clock resolution: ~%s (min tick width across 20 observed tick transitions)\n", clockResolution)
	fmt.Fprintf(&sb, "Latency, post-warm-up (microseconds):\n")
	fmt.Fprintf(&sb, "  p50: %8.2f\n", float64(p50)/1000)
	fmt.Fprintf(&sb, "  p95: %8.2f\n", float64(p95)/1000)
	fmt.Fprintf(&sb, "  p99: %8.2f\n", float64(p99)/1000)
	fmt.Fprintf(&sb, "  max: %8.2f\n", float64(maxLatency)/1000)
	if clockResolution > 10*time.Microsecond {
		fmt.Fprintf(&sb, "  WARNING: clock resolution above is far coarser than typical native hardware\n")
		fmt.Fprintf(&sb, "  (~100ns-1us via QueryPerformanceCounter). SubmitOrder's real per-call cost\n")
		fmt.Fprintf(&sb, "  (~1us, see the sequential go test benchmark) is smaller than one tick here, so\n")
		fmt.Fprintf(&sb, "  most per-call reads above are quantized to 0 and the rest jump to whole tick\n")
		fmt.Fprintf(&sb, "  widths - this looks like a virtualized/sandboxed clock source, not real engine\n")
		fmt.Fprintf(&sb, "  behavior. Trust throughput/orders-per-sec above, and ns/op from\n")
		fmt.Fprintf(&sb, "  `go test -bench=. ./internal/orderbook`, more than these percentiles until this\n")
		fmt.Fprintf(&sb, "  is re-run on real (non-virtualized) hardware.\n")
	}
	fmt.Fprintf(&sb, "\n")
	fmt.Fprintf(&sb, "GC during run:\n")
	fmt.Fprintf(&sb, "  GC cycles:      %d\n", gcCycles)
	fmt.Fprintf(&sb, "  Total GC pause: %s\n", gcPause)
	fmt.Fprintf(&sb, "\n")
	fmt.Fprintf(&sb, "Runtime environment:\n")
	fmt.Fprintf(&sb, "  GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
	fmt.Fprintf(&sb, "  NumCPU:     %d\n", runtime.NumCPU())
	fmt.Fprintf(&sb, "  GOOS/ARCH:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&sb, "  Go version: %s\n", runtime.Version())

	report := sb.String()
	fmt.Print(report)

	if err := os.MkdirAll("benchmarks", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create benchmarks directory: %v\n", err)
		os.Exit(1)
	}
	filename := filepath.Join("benchmarks", fmt.Sprintf("run_%s.txt", time.Now().Format("20060102_150405")))
	if err := os.WriteFile(filename, []byte(report), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write report file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nReport written to %s\n", filename)
}
