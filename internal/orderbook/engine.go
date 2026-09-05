package orderbook

import (
	"context"
	"errors"
)

// ErrEngineStopped is returned by an Engine method when the request could
// not be delivered to (or answered by) the owning goroutine because Stop
// has already been called, or Start never was.
var ErrEngineStopped = errors.New("orderbook: engine is stopped")

// engineRequest is one unit of work for Engine's owning goroutine to run.
// SubmitOrder, CancelOrder, BestBid, and BestAsk each get their own
// concrete type below rather than being folded into one struct with a
// "kind" tag and a pile of mostly-unused fields — every variant has a
// genuinely different payload and a genuinely different response shape
// (trades+error vs. just error vs. price+bool), so giving each its own
// small execute method reads better than a switch statement branching on
// a kind enum. Method dispatch *is* the switch.
type engineRequest interface {
	// execute runs the request against book and sends the result on the
	// request's own response channel. It only ever runs on Engine's single
	// owning goroutine (see Engine.run), which is what lets it touch book
	// directly with no lock: there is never a second goroutine that could
	// be touching it at the same time.
	execute(book *OrderBook)
}

type submitOrderRequest struct {
	order Order
	resp  chan submitOrderResult
}

type submitOrderResult struct {
	trades []Trade
	err    error
}

func (r *submitOrderRequest) execute(book *OrderBook) {
	trades, err := book.SubmitOrder(r.order)
	r.resp <- submitOrderResult{trades: trades, err: err}
}

type cancelOrderRequest struct {
	orderID string
	resp    chan error
}

func (r *cancelOrderRequest) execute(book *OrderBook) {
	r.resp <- book.CancelOrder(r.orderID)
}

// bestPriceResult is shared by the BestBid and BestAsk request/response
// pair below — both just report a price and whether one exists.
type bestPriceResult struct {
	price  int64
	exists bool
}

type bestBidRequest struct {
	resp chan bestPriceResult
}

func (r *bestBidRequest) execute(book *OrderBook) {
	price, exists := book.BestBid()
	r.resp <- bestPriceResult{price: price, exists: exists}
}

type bestAskRequest struct {
	resp chan bestPriceResult
}

func (r *bestAskRequest) execute(book *OrderBook) {
	price, exists := book.BestAsk()
	r.resp <- bestPriceResult{price: price, exists: exists}
}

// Engine wraps a single OrderBook and makes it safe to call from many
// goroutines concurrently — a real simulation will have dozens of
// independent trading agents all submitting/cancelling orders at once.
//
// It does this with a single-writer goroutine, not a mutex around
// OrderBook's methods. Every request — SubmitOrder, CancelOrder, BestBid,
// BestAsk — is packaged up and sent over the unbuffered requests channel;
// exactly one goroutine (run, launched by Start) ever receives from that
// channel and is therefore the only goroutine that ever touches the
// underlying OrderBook. Because only one goroutine ever touches it, no
// lock is needed at all — this is Go's "share memory by communicating"
// idea applied directly: instead of many goroutines sharing the book and
// coordinating access to it with a Mutex, they hand their work to the one
// goroutine that owns it and get an answer back on a private channel.
//
// The channel being unbuffered is deliberate too: a caller's send only
// completes once run is actually ready to receive it, which means requests
// are naturally serialized in true arrival order with no extra queueing
// data structure, and a slow/blocked owning goroutine applies backpressure
// to callers for free.
//
// This will later be benchmarked against a mutex-protected OrderBook as
// part of the portfolio's performance comparison — that alternative is
// intentionally not implemented yet (see CLAUDE.md).
type Engine struct {
	book     *OrderBook
	requests chan engineRequest

	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{} // closed once run has actually exited
}

// NewEngine creates an Engine for the given symbol. Call Start before
// issuing any requests.
func NewEngine(symbol string) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		book:     NewOrderBook(symbol),
		requests: make(chan engineRequest),
		ctx:      ctx,
		cancel:   cancel,
		stopped:  make(chan struct{}),
	}
}

// Start launches the single goroutine that owns the order book. It must be
// called exactly once, before any of Engine's request methods are used.
func (e *Engine) Start() {
	go e.run()
}

// run is the one goroutine that ever touches e.book. It loops until Stop
// cancels its context, executing whatever request arrives in the meantime.
func (e *Engine) run() {
	defer close(e.stopped)
	for {
		select {
		case req := <-e.requests:
			req.execute(e.book)
		case <-e.ctx.Done():
			return
		}
	}
}

// Stop signals the owning goroutine to shut down and blocks until it has
// actually exited, so callers can rely on the book being quiescent as soon
// as Stop returns. Safe to call once Start has been called; callers should
// stop issuing new requests before calling Stop rather than racing the two —
// requests still in flight when Stop is called either complete normally or
// get ErrEngineStopped (see send), never a panic or a hang.
func (e *Engine) Stop() {
	e.cancel()
	<-e.stopped
}

// send hands req to the owning goroutine. It returns ErrEngineStopped
// instead of blocking forever on a request nobody will ever read if the
// engine has already been stopped (or Start was never called).
func (e *Engine) send(req engineRequest) error {
	select {
	case e.requests <- req:
		return nil
	case <-e.ctx.Done():
		return ErrEngineStopped
	}
}

// SubmitOrder submits an order to the book, blocking until the owning
// goroutine has processed it. Callers see an ordinary blocking method call;
// the channel handoff to the owning goroutine is an implementation detail.
func (e *Engine) SubmitOrder(order Order) ([]Trade, error) {
	resp := make(chan submitOrderResult, 1)
	if err := e.send(&submitOrderRequest{order: order, resp: resp}); err != nil {
		return nil, err
	}
	// send returning nil means the owning goroutine already received the
	// request (the channel is unbuffered) and will run execute, which is
	// guaranteed to send exactly one result here — no need to also select
	// on e.ctx.Done() below.
	result := <-resp
	return result.trades, result.err
}

// CancelOrder cancels a resting order by ID, blocking until the owning
// goroutine has processed it.
func (e *Engine) CancelOrder(orderID string) error {
	resp := make(chan error, 1)
	if err := e.send(&cancelOrderRequest{orderID: orderID, resp: resp}); err != nil {
		return err
	}
	return <-resp
}

// BestBid returns the current best bid price. If the engine has already
// been stopped (or never started), it returns (0, false) rather than
// blocking forever — indistinguishable from "no resting bids," which is a
// reasonable answer for a book nobody is running anymore.
func (e *Engine) BestBid() (price int64, exists bool) {
	resp := make(chan bestPriceResult, 1)
	if err := e.send(&bestBidRequest{resp: resp}); err != nil {
		return 0, false
	}
	result := <-resp
	return result.price, result.exists
}

// BestAsk returns the current best ask price. See BestBid for the stopped-
// engine behavior.
func (e *Engine) BestAsk() (price int64, exists bool) {
	resp := make(chan bestPriceResult, 1)
	if err := e.send(&bestAskRequest{resp: resp}); err != nil {
		return 0, false
	}
	result := <-resp
	return result.price, result.exists
}
