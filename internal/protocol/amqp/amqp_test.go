package amqp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mockmint/mockmint/internal/pkg"
)

const ordersEvents = "../../../examples/orders-events"

// loadPkg loads the orders-events example package.
func loadPkg(t *testing.T) *pkg.Package {
	t.Helper()
	pkgs, err := pkg.LoadPath(context.Background(), ordersEvents, pkg.Defaults{Validation: "warn"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pkgs[0]
}

func loadFS(t *testing.T, files map[string]string) *pkg.Package {
	t.Helper()
	fsys := fstest.MapFS{}
	for k, v := range files {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	p, err := pkg.Load(context.Background(), fsys, "test", pkg.Defaults{Validation: "strict"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func op(t *testing.T, p *pkg.Package, id string) *pkg.AsyncOperation {
	t.Helper()
	for _, o := range p.Async.Operations {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("operation %s not found", id)
	return nil
}

func reply(t *testing.T, out Outcome) map[string]any {
	t.Helper()
	if out.Verdict != Ack || out.Reply == nil {
		t.Fatalf("outcome = %+v", out)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Reply.Msg.Body, &m); err != nil {
		t.Fatalf("reply body %s: %v", out.Reply.Msg.Body, err)
	}
	return m
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestHandleRequestReply(t *testing.T) {
	p := loadPkg(t)
	place := op(t, p, "placeOrder")
	d := Delivery{RoutingKey: "orders.requests", ReplyTo: "amq.rabbitmq.reply-to", CorrelationID: "c-1", ContentType: "application/json"}

	d.Body = []byte(`{"sku":"WIDGET-9","qty":500}`)
	out := Handle(p, place, d)
	if m := reply(t, out); m["status"] != "rejected" || out.Example != "bulk" {
		t.Fatalf("bulk reply = %v (%s)", m, out.Example)
	}
	if out.Reply.RoutingKey != "amq.rabbitmq.reply-to" || out.Reply.Exchange != "" || out.Reply.Msg.CorrelationId != "c-1" {
		t.Fatalf("reply routing = %+v", out.Reply)
	}
	if out.Delay < 5*time.Millisecond || out.Delay > 20*time.Millisecond {
		t.Fatalf("latency = %v", out.Delay)
	}

	d.Body = []byte(`{"sku":"WIDGET-9","qty":1}`)
	out = Handle(p, place, d)
	m := reply(t, out)
	if m["status"] != "accepted" || m["sku"] != "WIDGET-9" || !uuidRE.MatchString(m["orderId"].(string)) {
		t.Fatalf("templated reply = %v", m)
	}
	if id, _ := out.Reply.Msg.Headers["x-order-id"].(string); !uuidRE.MatchString(id) {
		t.Fatalf("templated header = %v", out.Reply.Msg.Headers)
	}
	// Seeded: the same request renders the same reply.
	if again := Handle(p, place, d); string(again.Reply.Msg.Body) != string(out.Reply.Msg.Body) || again.Reply.Msg.MessageId != out.Reply.Msg.MessageId {
		t.Fatal("seeded replies differ for identical requests")
	}

	d.Body = []byte(`{"sku":"GADGET","qty":3}`)
	if out = Handle(p, place, d); out.Example != "accepted" {
		t.Fatalf("catch-all example = %q", out.Example)
	}

	// No correlation id: fall back to the request's message id.
	d.CorrelationID, d.MessageID = "", "m-9"
	if out = Handle(p, place, d); out.Reply.Msg.CorrelationId != "m-9" {
		t.Fatalf("correlation fallback = %q", out.Reply.Msg.CorrelationId)
	}
}

func TestHandleStrictRejects(t *testing.T) {
	p := loadPkg(t)
	place := op(t, p, "placeOrder")
	for body, want := range map[string]string{
		`{"sku":"X","qty":0}`: "invalid message: /qty",
		`not json`:            "body is not JSON",
		``:                    "body is not JSON",
	} {
		out := Handle(p, place, Delivery{Body: []byte(body), ReplyTo: "r"})
		if out.Verdict != Reject || !strings.Contains(out.Reason, want) {
			t.Errorf("%q: outcome = %+v, want reject %q", body, out, want)
		}
	}
	sink := op(t, p, "consumePayments")
	if out := Handle(p, sink, Delivery{Body: []byte(`{"orderId":"o","amount":-1}`)}); out.Verdict != Reject {
		t.Fatalf("sink accepted invalid payment: %+v", out)
	}
	if out := Handle(p, sink, Delivery{Body: []byte(`{"orderId":"o","amount":5}`)}); out.Verdict != Ack || out.Reply != nil {
		t.Fatalf("sink = %+v", out)
	}
}

const jobsSpec = `asyncapi: 3.0.0
info: {title: jobs, version: "1"}
channels:
  req:
    address: jobs
    bindings: {amqp: {is: queue, queue: {name: jobs}}}
    messages: {Job: {payload: {type: object, properties: {kind: {type: string}}}}}
  done:
    address: jobs.done
    bindings: {amqp: {is: routingKey, exchange: {name: jobs.x, type: direct}}}
    messages:
      Done:
        payload: {type: object, required: [ok], properties: {ok: {type: boolean}}}
        examples:
          - {name: ok, payload: {ok: true}}
          - {name: bad, payload: {ok: "{{.Request.Body.kind}}"}}
operations:
  run:
    action: receive
    channel: {$ref: "#/channels/req"}
    reply: {channel: {$ref: "#/channels/done"}}
`

func TestHandleFixedReplyChannelAndOutgoingValidation(t *testing.T) {
	p := loadFS(t, map[string]string{
		"asyncapi.yaml": jobsSpec,
		"mockmint.yaml": `async:
  operations:
    run:
      dispatcher:
        type: body_jsonpath
        rules: [{when: {"$.kind": weird}, example: bad}]
        default: ok
`,
	})
	run := op(t, p, "run")
	out := Handle(p, run, Delivery{Body: []byte(`{"kind":"x"}`)}) // no reply_to
	if out.Reply == nil || out.Reply.Exchange != "jobs.x" || out.Reply.RoutingKey != "jobs.done" {
		t.Fatalf("fixed reply channel = %+v", out)
	}
	out = Handle(p, run, Delivery{Body: []byte(`{"kind":"weird"}`), ReplyTo: "r"})
	if out.Verdict != Reject || !strings.Contains(out.Reason, `reply example "bad" is invalid`) {
		t.Fatalf("invalid reply not rejected: %+v", out)
	}
}

func TestHandleFaults(t *testing.T) {
	for action, check := range map[string]func(Outcome) bool{
		"drop":       func(o Outcome) bool { return o.Verdict == Ack && o.Reply == nil && strings.Contains(o.Reason, "drop") },
		"deadletter": func(o Outcome) bool { return o.Verdict == Reject && strings.Contains(o.Reason, "deadletter") },
	} {
		p := loadFS(t, map[string]string{
			"asyncapi.yaml": jobsSpec,
			"mockmint.yaml": "async: {behavior: {faults: [{probability: 1, action: " + action + "}]}}\n",
		})
		if out := Handle(p, op(t, p, "run"), Delivery{Body: []byte(`{}`), ReplyTo: "r"}); !check(out) {
			t.Errorf("%s: outcome = %+v", action, out)
		}
	}
}

func TestRenderSend(t *testing.T) {
	p := loadPkg(t)
	pub := op(t, p, "publishOrderCreated")
	a, err := Render(p, pub, "widget", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Exchange != "orders.events" || a.RoutingKey != "order.created" || a.Msg.DeliveryMode != 2 || a.Msg.ContentType != "application/json" {
		t.Fatalf("outgoing = %+v", a)
	}
	var m map[string]any
	if err := json.Unmarshal(a.Msg.Body, &m); err != nil || m["createdAt"] != "2025-01-01T12:00:00.000Z" || !uuidRE.MatchString(m["orderId"].(string)) {
		t.Fatalf("body = %s", a.Msg.Body)
	}
	b, _ := Render(p, pub, "widget", 2)
	a2, _ := Render(p, pub, "widget", 1)
	if string(a.Msg.Body) == string(b.Msg.Body) || string(a.Msg.Body) != string(a2.Msg.Body) {
		t.Fatal("seq must vary output deterministically")
	}
	if _, err := Render(p, pub, "nope", 1); err == nil || !strings.Contains(err.Error(), "gadget, widget") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlan(t *testing.T) {
	topo, err := Plan([]*pkg.Package{loadPkg(t)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range topo.Exchanges {
		names = append(names, e.Name+":"+e.Kind)
	}
	for _, q := range topo.Queues {
		names = append(names, "q:"+q.Name+":dlx="+q.DeadLetter)
	}
	for _, b := range topo.Bindings {
		names = append(names, "b:"+b.Queue+"<-"+b.Exchange+"/"+b.Key)
	}
	want := "orders.dlx:fanout orders.events:topic payments.events:topic " +
		"q:orders.dlx.queue:dlx= q:orders.payments:dlx=orders.dlx q:orders.requests:dlx=orders.dlx " +
		"b:orders.dlx.queue<-orders.dlx/ b:orders.payments<-payments.events/payment.#"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("topology:\n got %s\nwant %s", got, want)
	}

	rec := &recorder{}
	if err := topo.Apply(rec); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.calls, "\n") != strings.Join([]string{
		"exchange orders.dlx fanout durable=true",
		"exchange orders.events topic durable=true",
		"exchange payments.events topic durable=true",
		"queue orders.dlx.queue durable=true args=map[]",
		"queue orders.payments durable=true args=map[x-dead-letter-exchange:orders.dlx]",
		"queue orders.requests durable=true args=map[x-dead-letter-exchange:orders.dlx]",
		"bind orders.dlx.queue orders.dlx ",
		"bind orders.payments payments.events payment.#",
	}, "\n") {
		t.Fatalf("apply calls:\n%s", strings.Join(rec.calls, "\n"))
	}
}

func TestPlanConflictsAndDeclareOff(t *testing.T) {
	a := loadPkg(t)
	b := loadFS(t, map[string]string{
		"asyncapi.yaml": strings.Replace(jobsSpec, "{is: queue, queue: {name: jobs}}", "{is: queue, queue: {name: orders.requests, durable: false}}", 1),
	})
	if _, err := Plan([]*pkg.Package{a, b}); err == nil || !strings.Contains(err.Error(), `queue "orders.requests" is declared differently`) {
		t.Fatalf("err = %v", err)
	}
	b.Async.Declare = false
	if _, err := Plan([]*pkg.Package{a, b}); err != nil {
		t.Fatalf("declare:false package still planned: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	lo, hi := 100*time.Millisecond, time.Second
	for attempt := range 40 {
		for range 50 {
			d := backoff(attempt, lo, hi)
			if d < lo || d > hi || (attempt < 3 && d > lo<<attempt) {
				t.Fatalf("attempt %d: %v out of range", attempt, d)
			}
		}
	}
}

func TestRedact(t *testing.T) {
	if got := redact("amqp://user:s3cret@host:5672/vh"); strings.Contains(got, "s3cret") {
		t.Fatalf("password leaked: %s", got)
	}
}

func TestPublishErrors(t *testing.T) {
	e, err := New([]*pkg.Package{loadPkg(t)}, Options{ConfirmTimeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.Publish(ctx, "orders-events", "publishOrderCreated", "widget"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("disconnected publish err = %v", err)
	}
	for _, tt := range []struct{ pkg, op, want string }{
		{"nope", "x", "no package"},
		{"orders-events", "nope", "no async operation"},
		{"orders-events", "placeOrder", "only send operations"},
	} {
		if err := e.Publish(ctx, tt.pkg, tt.op, "widget"); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s/%s: err = %v", tt.pkg, tt.op, err)
		}
	}
	if !e.HasOperations() || e.Connected() {
		t.Fatal("engine state wrong")
	}
}

type recorder struct{ calls []string }

func (r *recorder) ExchangeDeclare(name, kind string, durable, _, _, _ bool, _ amqp.Table) error {
	r.calls = append(r.calls, "exchange "+name+" "+kind+" durable="+boolStr(durable))
	return nil
}

func (r *recorder) ExchangeDeclarePassive(name, _ string, _, _, _, _ bool, _ amqp.Table) error {
	r.calls = append(r.calls, "check exchange "+name)
	return nil
}

func (r *recorder) QueueDeclare(name string, durable, _, _, _ bool, args amqp.Table) (amqp.Queue, error) {
	r.calls = append(r.calls, "queue "+name+" durable="+boolStr(durable)+" args="+fmtTable(args))
	return amqp.Queue{Name: name}, nil
}

func (r *recorder) QueueBind(name, key, exchange string, _ bool, _ amqp.Table) error {
	r.calls = append(r.calls, "bind "+name+" "+exchange+" "+key)
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func fmtTable(t amqp.Table) string {
	if t == nil {
		return "map[]"
	}
	var parts []string
	for k, v := range t {
		parts = append(parts, k+":"+v.(string))
	}
	return "map[" + strings.Join(parts, " ") + "]"
}

func TestHandleRejectsUnanswerableRequest(t *testing.T) {
	p := loadPkg(t)
	// placeOrder replies to reply_to only (reply address null).
	out := Handle(p, op(t, p, "placeOrder"), Delivery{Body: []byte(`{"sku":"WIDGET-9","qty":1}`)})
	if out.Verdict != Reject || !strings.Contains(out.Reason, "no reply_to") {
		t.Fatalf("outcome = %+v", out)
	}
	if out = Handle(p, op(t, p, "placeOrder"), Delivery{Body: []byte(`{"sku":"WIDGET-9","qty":1}`), ReplyTo: "q"}); !out.Reply.Mandatory {
		t.Fatal("replies must be mandatory so unroutable ones come back")
	}
}

func TestHandleValidatesHeaders(t *testing.T) {
	spec := strings.Replace(jobsSpec,
		"messages: {Job: {payload: {type: object, properties: {kind: {type: string}}}}}",
		"messages: {Job: {headers: {type: object, required: [x-tenant], properties: {x-tenant: {type: string}, x-retries: {type: integer}}}, payload: {type: object}}}", 1)
	p := loadFS(t, map[string]string{
		"asyncapi.yaml": spec,
		"mockmint.yaml": "async: {operations: {run: {dispatcher: {type: static, example: ok}}}}\n",
	})
	run := op(t, p, "run")
	out := Handle(p, run, Delivery{Body: []byte(`{}`), ReplyTo: "r", Headers: amqp.Table{"x-retries": int32(1)}})
	if out.Verdict != Reject || !strings.Contains(out.Reason, "headers") || !strings.Contains(out.Reason, "x-tenant") {
		t.Fatalf("missing header accepted: %+v", out)
	}
	out = Handle(p, run, Delivery{Body: []byte(`{}`), ReplyTo: "r", Headers: amqp.Table{"x-tenant": "acme", "x-retries": int32(2)}})
	if out.Verdict != Ack {
		t.Fatalf("valid headers rejected (int32 must validate as integer): %+v", out)
	}
}

func TestPlanReplyExchangeFromBinding(t *testing.T) {
	// jobs: the receive operation replies on jobs.x, a direct exchange.
	p := loadFS(t, map[string]string{"asyncapi.yaml": jobsSpec})
	topo, err := Plan([]*pkg.Package{p})
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Exchanges) != 1 || topo.Exchanges[0].Name != "jobs.x" || topo.Exchanges[0].Kind != "direct" {
		t.Fatalf("reply exchange = %+v", topo.Exchanges)
	}
	// A send operation declaring the same exchange identically is no conflict.
	both := strings.Replace(jobsSpec, "operations:\n", "operations:\n  finish:\n    action: send\n    channel: {$ref: \"#/channels/done\"}\n", 1)
	if _, err := Plan([]*pkg.Package{loadFS(t, map[string]string{"asyncapi.yaml": both})}); err != nil {
		t.Fatalf("consistent reply/send exchange reported as conflict: %v", err)
	}
}

func TestPlanExchangeOverrideIsChecked(t *testing.T) {
	p := loadFS(t, map[string]string{
		"asyncapi.yaml": strings.Replace(jobsSpec, "operations:\n", "operations:\n  finish:\n    action: send\n    channel: {$ref: \"#/channels/done\"}\n", 1),
		"mockmint.yaml": "async: {operations: {finish: {exchange: legacy.x}}}\n",
	})
	topo, err := Plan([]*pkg.Package{p})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if err := topo.Apply(rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rec.calls, "\n"), "check exchange legacy.x") {
		t.Fatalf("override exchange not checked:\n%s", strings.Join(rec.calls, "\n"))
	}
}

func TestDrainReturnsStopsOnClosedChannel(t *testing.T) {
	c := make(chan amqp.Return, 1)
	c <- amqp.Return{}
	close(c) // amqp091 closes NotifyReturn channels when the channel shuts down
	done := make(chan struct{})
	go func() { drainReturns(c); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drainReturns spins on a closed channel")
	}
}
