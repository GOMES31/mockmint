package amqp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/observability"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/spec/asyncapi"
)

// Options configure the engine.
type Options struct {
	URL            string
	Prefetch       int
	Heartbeat      time.Duration
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	ConfirmTimeout time.Duration

	// Traffic and Metrics are optional.
	Traffic *observability.Traffic
	Metrics *observability.Metrics
}

// ErrNotConnected is returned by Publish while the broker is unreachable.
var ErrNotConnected = errors.New("not connected to RabbitMQ")

// ErrUnroutable is returned when the broker returns a mandatory message
// because no queue is bound to its destination.
var ErrUnroutable = errors.New("message is unroutable")

// Engine runs a set of packages' async operations against one broker. It
// reconnects on failure; create a new Engine to change the packages.
type Engine struct {
	pkgs []*pkg.Package
	opts Options
	log  *slog.Logger
	topo *Topology

	connected atomic.Bool
	// pubMu guards pub and serializes publish+confirm, so a basic.return
	// (delivered before its ack, in wire order) belongs to the publish in
	// flight. Replies are therefore not pipelined; fine for a mock.
	pubMu   sync.Mutex
	pub     *amqp.Channel
	returns chan amqp.Return
	seq     sync.Map // "pkg/op" → *atomic.Uint64, publish counters
}

// New plans the topology for pkgs. It fails on conflicting declarations.
func New(pkgs []*pkg.Package, opts Options, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	topo, err := Plan(pkgs)
	if err != nil {
		return nil, err
	}
	return &Engine{pkgs: pkgs, opts: opts, log: log.With("component", "amqp"), topo: topo}, nil
}

// Connected reports whether a broker session is up.
func (e *Engine) Connected() bool { return e.connected.Load() }

func (e *Engine) setConnected(up bool) {
	e.connected.Store(up)
	if m := e.opts.Metrics; m != nil {
		v := 0.0
		if up {
			v = 1
		}
		m.AMQPUp.Set(v)
	}
}

// HasOperations reports whether any package has async operations.
func (e *Engine) HasOperations() bool {
	for _, p := range e.pkgs {
		if p.Async != nil && len(p.Async.Operations) > 0 {
			return true
		}
	}
	return false
}

// Run connects and serves until ctx is done, reconnecting with exponential
// backoff and full jitter whenever the connection or a channel fails. Broker
// failures are logged and retried, never returned.
func (e *Engine) Run(ctx context.Context) {
	attempt := 0
	for {
		conn, err := amqp.DialConfig(e.opts.URL, amqp.Config{
			Heartbeat:  e.opts.Heartbeat,
			Properties: amqp.Table{"connection_name": "mockmint"},
		})
		if err == nil {
			attempt = 0
			e.log.Info("connected", "url", redact(e.opts.URL))
			err = e.session(ctx, conn)
			_ = conn.Close()
			e.setConnected(false)
		}
		if ctx.Err() != nil {
			return
		}
		wait := backoff(attempt, e.opts.ReconnectMin, e.opts.ReconnectMax)
		attempt++
		e.log.Warn("broker unavailable; reconnecting", "error", err, "in", wait.String(), "attempt", attempt)
		if behavior.Sleep(ctx, wait) != nil {
			return
		}
	}
}

// backoff returns a delay in [0, min(max, lo·2^attempt)] (full jitter), but
// never below lo.
func backoff(attempt int, lo, hi time.Duration) time.Duration {
	ceil := hi
	if attempt < 30 {
		ceil = min(hi, lo<<attempt)
	}
	return max(lo, time.Duration(rand.Int64N(int64(ceil)+1))) //nolint:gosec // G404: jitter, not security
}

// session serves one connection until it fails or ctx is done.
func (e *Engine) session(ctx context.Context, conn *amqp.Connection) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup

	setup, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := e.topo.Apply(setup); err != nil {
		return err
	}
	_ = setup.Close()

	pub, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := pub.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	returns := pub.NotifyReturn(make(chan amqp.Return, 1))
	pubClosed := pub.NotifyClose(make(chan *amqp.Error, 1))
	e.setPublisher(pub, returns)
	defer e.setPublisher(nil, nil)

	errc := make(chan error, 1)
	fail := func(err error) {
		select {
		case errc <- err:
		default:
		}
	}
	// A channel exception (e.g. publishing to a missing exchange) closes
	// the publisher channel; restart the session rather than failing every
	// later publish.
	wg.Go(func() {
		select {
		case cerr, ok := <-pubClosed:
			if ok && cerr != nil {
				fail(fmt.Errorf("publisher channel closed: %w", cerr))
			}
		case <-sctx.Done():
		}
	})
	for _, p := range e.pkgs {
		if p.Async == nil {
			continue
		}
		for _, op := range p.Async.Operations {
			switch {
			case op.Action == asyncapi.ActionReceive:
				ch, deliveries, err := e.consume(sctx, conn, p, op)
				if err != nil {
					cancel() // stop consumers started so far before returning
					wg.Wait()
					return err
				}
				wg.Go(func() {
					e.serve(sctx, p, op, deliveries, fail)
					_ = ch.Close()
				})
			case op.Schedule != nil:
				wg.Go(func() { e.schedule(sctx, p, op) })
			}
		}
	}
	e.setConnected(true)

	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	select {
	case <-ctx.Done():
		err = nil
	case cerr := <-closed:
		err = errors.New("connection closed by the broker")
		if cerr != nil {
			err = fmt.Errorf("connection closed: %w", cerr)
		}
	case err = <-errc:
	}
	e.setConnected(false)
	cancel()
	wg.Wait()
	return err
}

func (e *Engine) setPublisher(ch *amqp.Channel, returns chan amqp.Return) {
	e.pubMu.Lock()
	defer e.pubMu.Unlock()
	if e.pub != nil && ch == nil {
		_ = e.pub.Close()
	}
	e.pub, e.returns = ch, returns
}

func (e *Engine) consume(ctx context.Context, conn *amqp.Connection, p *pkg.Package, op *pkg.AsyncOperation) (*amqp.Channel, <-chan amqp.Delivery, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, err
	}
	if err := ch.Qos(e.opts.Prefetch, 0, false); err != nil {
		return nil, nil, err
	}
	tag := "mockmint." + p.Name + "." + op.ID
	deliveries, err := ch.ConsumeWithContext(ctx, op.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("consume %q for %s/%s: %w", op.Queue, p.Name, op.ID, err)
	}
	e.log.Info("consuming", "package", p.Name, "operation", op.ID, "queue", op.Queue, "replies", op.Replies != nil)
	return ch, deliveries, nil
}

// serve handles deliveries concurrently (bounded by the prefetch window)
// and reports a channel failure unless ctx ended it.
func (e *Engine) serve(ctx context.Context, p *pkg.Package, op *pkg.AsyncOperation, deliveries <-chan amqp.Delivery, fail func(error)) {
	var inflight sync.WaitGroup
	defer inflight.Wait()
	for d := range deliveries {
		inflight.Go(func() { e.handle(ctx, p, op, d) })
	}
	if ctx.Err() == nil {
		fail(fmt.Errorf("consumer %s/%s stopped (channel closed)", p.Name, op.ID))
	}
}

func (e *Engine) handle(ctx context.Context, p *pkg.Package, op *pkg.AsyncOperation, d amqp.Delivery) {
	start := time.Now()
	log := e.log.With("package", p.Name, "operation", op.ID, "messageId", d.MessageId, "correlationId", d.CorrelationId)
	out := Handle(p, op, Delivery{
		Exchange: d.Exchange, RoutingKey: d.RoutingKey, Body: d.Body, ContentType: d.ContentType,
		CorrelationID: d.CorrelationId, ReplyTo: d.ReplyTo, MessageID: d.MessageId, Headers: d.Headers,
	})
	outcome := "ack"
	var replyBody []byte
	defer func() {
		if m := e.opts.Metrics; m != nil {
			m.AMQPMessages.Inc(p.Name, op.ID, outcome)
		}
		e.opts.Traffic.Record(observability.Exchange{
			Time: start, Protocol: "amqp", Package: p.Name, Operation: op.ID, Example: out.Example,
			Duration: time.Since(start), Path: d.RoutingKey, Outcome: outcome,
			Headers: tableStrings(d.Headers), Body: string(d.Body), RespBody: string(replyBody),
		})
	}()
	for _, w := range out.Warnings {
		log.Warn("validation", "warning", w)
	}
	if behavior.Sleep(ctx, out.Delay) != nil {
		outcome = "requeue"
		_ = d.Nack(false, true) // shutting down: hand the message back
		return
	}
	if out.Verdict == Reject {
		outcome = "reject"
		log.Warn("message rejected", "reason", out.Reason)
		_ = d.Nack(false, false)
		return
	}
	if out.Reply != nil {
		err := e.publish(ctx, out.Reply)
		switch {
		case errors.Is(err, ErrUnroutable):
			// Requeueing would loop forever if the caller's reply queue is
			// gone; dead-letter the request instead.
			outcome = "reject"
			log.Warn("reply unroutable; rejecting request", "error", err, "replyTo", out.Reply.RoutingKey)
			_ = d.Nack(false, false)
			return
		case err != nil:
			outcome = "requeue"
			log.Warn("reply failed; requeueing request", "error", err)
			_ = d.Nack(false, true)
			return
		}
		replyBody = out.Reply.Msg.Body
		log.Debug("replied", "example", out.Example, "to", out.Reply.RoutingKey)
	} else if out.Reason != "" {
		log.Info("no reply", "reason", out.Reason)
	}
	if err := d.Ack(false); err != nil {
		log.Warn("ack failed", "error", err)
	}
}

// publish sends o, waits for the broker's confirm, and reports a mandatory
// message the broker returned as ErrUnroutable.
func (e *Engine) publish(ctx context.Context, o *Outgoing) error {
	e.pubMu.Lock()
	defer e.pubMu.Unlock()
	ch := e.pub
	if ch == nil {
		return ErrNotConnected
	}
	drainReturns(e.returns) // stale returns from a publish that timed out
	dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, o.Exchange, o.RoutingKey, o.Mandatory, false, o.Msg)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, e.opts.ConfirmTimeout)
	defer cancel()
	acked, err := dc.WaitContext(cctx)
	if err != nil {
		return fmt.Errorf("waiting for publisher confirm: %w", err)
	}
	if !acked {
		return errors.New("broker nacked the publish")
	}
	// The broker sends basic.return before basic.ack, and amqp091 hands it
	// to the returns channel before resolving the confirm.
	// The library closes the returns channel when the channel shuts down;
	// a closed channel is not a return.
	select {
	case r, ok := <-e.returns:
		if ok {
			return fmt.Errorf("%w: %s (exchange %q, routing key %q)", ErrUnroutable, r.ReplyText, r.Exchange, r.RoutingKey)
		}
	default:
	}
	return nil
}

// drainReturns discards returns left over from a publish whose confirm
// timed out. It stops at an empty or closed channel.
func drainReturns(c chan amqp.Return) {
	for {
		select {
		case _, ok := <-c:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// schedule publishes op's examples every interval, cycling through them.
func (e *Engine) schedule(ctx context.Context, p *pkg.Package, op *pkg.AsyncOperation) {
	s := op.Schedule
	if behavior.Sleep(ctx, s.InitialDelay.D()) != nil {
		return
	}
	t := time.NewTicker(s.Interval.D())
	defer t.Stop()
	for i := 0; ; i++ {
		example := s.Examples[i%len(s.Examples)]
		if err := e.Publish(ctx, p.Name, op.ID, example); err != nil && ctx.Err() == nil {
			e.log.Warn("scheduled publish failed", "package", p.Name, "operation", op.ID, "example", example, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Publish renders example of the send operation opID in package pkgName and
// publishes it, waiting for the broker's confirm.
func (e *Engine) Publish(ctx context.Context, pkgName, opID, example string) error {
	p, op, err := e.sendOperation(pkgName, opID)
	if err != nil {
		return err
	}
	ctr, _ := e.seq.LoadOrStore(pkgName+"/"+opID, new(atomic.Uint64))
	o, err := Render(p, op, example, ctr.(*atomic.Uint64).Add(1))
	if err != nil {
		return err
	}
	err = e.publish(ctx, o)
	if m := e.opts.Metrics; m != nil {
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.AMQPPublish.Inc(pkgName, opID, result)
	}
	if err != nil {
		return err
	}
	e.opts.Traffic.Record(observability.Exchange{
		Time: time.Now(), Protocol: "amqp", Package: pkgName, Operation: opID, Example: example,
		Path: o.RoutingKey, Outcome: "published", RespBody: string(o.Msg.Body),
	})
	e.log.Debug("published", "package", pkgName, "operation", opID, "example", example, "exchange", o.Exchange, "routingKey", o.RoutingKey)
	return nil
}

func (e *Engine) sendOperation(pkgName, opID string) (*pkg.Package, *pkg.AsyncOperation, error) {
	for _, p := range e.pkgs {
		if p.Name != pkgName || p.Async == nil {
			continue
		}
		for _, op := range p.Async.Operations {
			if op.ID != opID {
				continue
			}
			if op.Action != asyncapi.ActionSend {
				return nil, nil, fmt.Errorf("operation %s/%s receives messages; only send operations can publish", pkgName, opID)
			}
			return p, op, nil
		}
		return nil, nil, fmt.Errorf("package %s has no async operation %q", pkgName, opID)
	}
	return nil, nil, fmt.Errorf("no package %q with async operations", pkgName)
}

// Packages returns the packages the engine serves.
func (e *Engine) Packages() []*pkg.Package { return e.pkgs }

func tableStrings(t amqp.Table) map[string]string {
	if len(t) == 0 {
		return nil
	}
	out := make(map[string]string, len(t))
	for k, v := range t {
		out[k] = headerString(v)
	}
	return out
}

// redact hides the password in an AMQP URL for logs.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	return u.Redacted()
}
