package pkg

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mockmint/mockmint/internal/dispatch"
)

const ordersEvents = "../../examples/orders-events"

func asyncOp(t *testing.T, p *Package, id string) *AsyncOperation {
	t.Helper()
	for _, o := range p.Async.Operations {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("async operation %s not found", id)
	return nil
}

func TestLoadOrdersEvents(t *testing.T) {
	p := loadOne(t, ordersEvents)
	if p.Spec != nil || p.AsyncSpec == nil || p.Name != "orders-events" || p.Version != "1.0" || len(p.Operations) != 0 {
		t.Fatalf("package = %+v", p)
	}
	a := p.Async
	if !a.Declare || a.DeadLetter == nil || a.DeadLetter.Exchange != "orders.dlx" || a.DeadLetter.Queue != "orders.dlx.queue" {
		t.Fatalf("async = %+v", a)
	}

	place := asyncOp(t, p, "placeOrder")
	if place.Queue != "orders.requests" || place.BindExchange != "" || place.Validation != "strict" {
		t.Fatalf("placeOrder target = %q %q %s", place.Queue, place.BindExchange, place.Validation)
	}
	rs := place.Replies
	if rs == nil || rs.Exchange != "" || rs.RoutingKey != "" || rs.Fallback != nil {
		t.Fatalf("replies = %+v", rs)
	}
	if got := ExampleNames(rs.Templates); strings.Join(got, ",") != "accepted,bulk,single" {
		t.Fatalf("reply examples = %v", got)
	}
	single := rs.Templates["single"]
	if single.BodyTemplate == nil || len(single.HeaderTemplates) != 1 || single.HeaderTemplates[0].Name != "x-order-id" {
		t.Fatalf("single = %+v", single)
	}
	// auto dispatch: request examples pair with replies by name.
	for body, want := range map[string]string{
		`{"sku":"WIDGET-9","qty":500}`: "bulk",
		`{"sku":"WIDGET-9","qty":1}`:   "single",
		`{"sku":"OTHER","qty":3}`:      "accepted", // unpaired reply = catch-all
	} {
		got, ok, err := rs.Dispatcher.Dispatch(&dispatch.Request{Header: http.Header{}, Body: []byte(body)})
		if err != nil || (want == "" && ok) || (want != "" && got != want) {
			t.Errorf("%s → %q ok=%v err=%v, want %q", body, got, ok, err, want)
		}
	}

	pub := asyncOp(t, p, "publishOrderCreated")
	if pub.Exchange != "orders.events" || pub.RoutingKey != "order.created" || pub.Schedule == nil || pub.Schedule.Interval.D() != 5*time.Second {
		t.Fatalf("publish = %+v", pub)
	}
	if strings.Join(pub.Schedule.Examples, ",") != "widget,gadget" || pub.AMQP.DeliveryMode != 2 {
		t.Fatalf("schedule = %+v", pub.Schedule)
	}

	sink := asyncOp(t, p, "consumePayments")
	if sink.Replies != nil || sink.Queue != "orders.payments" || sink.BindExchange != "payments.events" || sink.BindKey != "payment.#" {
		t.Fatalf("sink = %+v", sink)
	}
}

const miniAsync = `asyncapi: 2.6.0
info: {title: Mini Async, version: "3"}
channels:
  jobs.submit:
    bindings: {amqp: {is: queue, queue: {name: jobs}}}
    publish:
      operationId: submitJob
      message:
        payload: {type: object, properties: {n: {type: integer}}}
        examples: [{name: big, payload: {n: 100}}]
  jobs.done:
    bindings: {amqp: {is: routingKey, exchange: {name: jobs.x, type: direct}}}
    subscribe:
      operationId: jobDone
      message:
        payload:
          type: object
          required: [ok]
          properties: {ok: {type: boolean}}
        examples: [{name: big, payload: {ok: false}}, {name: fine, payload: {ok: true}}]
  jobs.audit:
    subscribe:
      operationId: auditTrail
      message:
        payload: {type: object, required: [id], properties: {id: {type: string, format: uuid}}}
`

func TestAsyncReplyWithAndSynthesis(t *testing.T) {
	p, err := Load(context.Background(), fstest.MapFS{
		"asyncapi.yaml": {Data: []byte(miniAsync)},
		"mockmint.yaml": {Data: []byte("async:\n  operations:\n    submitJob: {replyWith: jobDone}\n")},
	}, "t", Defaults{Validation: "warn"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	submit := asyncOp(t, p, "submitJob")
	rs := submit.Replies
	if rs == nil || rs.Exchange != "jobs.x" || rs.RoutingKey != "jobs.done" {
		t.Fatalf("replyWith target = %+v", rs)
	}
	got, ok, _ := rs.Dispatcher.Dispatch(&dispatch.Request{Header: http.Header{}, Body: []byte(`{"n":100}`)})
	if !ok || got != "big" {
		t.Fatalf("paired dispatch = %q", got)
	}
	got, ok, _ = rs.Dispatcher.Dispatch(&dispatch.Request{Header: http.Header{}, Body: []byte(`{"n":1}`)})
	if !ok || got != "fine" {
		t.Fatalf("default dispatch = %q (unpaired example is the catch-all)", got)
	}

	audit := asyncOp(t, p, "auditTrail")
	g := audit.Templates["generated"]
	if g == nil || !strings.Contains(string(g.Body), `"id":"`) {
		t.Fatalf("synthesized example = %+v", g)
	}
	if audit.Exchange != "" && audit.RoutingKey != "jobs.audit" {
		t.Fatalf("default-exchange target = %q %q", audit.Exchange, audit.RoutingKey)
	}
}

func TestAsyncConfigErrors(t *testing.T) {
	for _, tt := range []struct{ manifest, want string }{
		{"async: {operations: {nope: {}}}", `"nope" matches no AsyncAPI operation`},
		{"async: {operations: {submitJob: {replyWith: auditTrail2}}}", "must name a send operation"},
		{"async: {operations: {submitJob: {replyWith: submitJob}}}", "must name a send operation"},
		{"async: {operations: {submitJob: {schedule: {interval: 1s}}}}", "apply to send operations"},
		{"async: {operations: {jobDone: {queue: q}}}", "apply to receive operations"},
		{"async: {operations: {jobDone: {schedule: {interval: 1ms}}}}", "at least 10ms"},
		{"async: {operations: {jobDone: {schedule: {interval: 1s, examples: [nope]}}}}", "unknown example"},
		{"async: {operations: {submitJob: {dispatcher: {type: static, example: big}}}}", "need a reply"},
		{"async: {operations: {submitJob: {replyWith: jobDone, fallback: nope}}}", "fallback: unknown example"},
		{"async: {behavior: {rateLimit: {rps: 1, burst: 1}}}", "HTTP operations only"},
		{"async: {behavior: {faults: [{probability: 1, status: 500}]}}", "drop or deadletter"},
		{"async: {deadLetter: {queue: x}}", "deadLetter.exchange is required"},
		{"async: {validation: loud}", "validation"},
		{"operations: {'GET /x': {}}", "no OpenAPI document"},
	} {
		_, err := Load(context.Background(), fstest.MapFS{
			"asyncapi.yaml": {Data: []byte(miniAsync)},
			"mockmint.yaml": {Data: []byte(tt.manifest + "\n")},
		}, "t", Defaults{}, nil)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.manifest, err, tt.want)
		}
	}
}

func TestHTTPRejectsDeadLetterFault(t *testing.T) {
	_, err := Load(context.Background(), fstest.MapFS{
		"openapi.yaml":  {Data: []byte(miniSpec)},
		"mockmint.yaml": {Data: []byte("behavior: {faults: [{probability: 1, action: deadletter}]}\n")},
	}, "t", Defaults{}, nil)
	if err == nil || !strings.Contains(err.Error(), "for async operations") {
		t.Fatalf("err = %v", err)
	}
}

func TestPackageWithBothSpecs(t *testing.T) {
	p, err := Load(context.Background(), fstest.MapFS{
		"openapi.yaml":  {Data: []byte(miniSpec)},
		"asyncapi.yaml": {Data: []byte(miniAsync)},
	}, "t", Defaults{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mini-api" || len(p.Operations) != 1 || len(p.Async.Operations) != 3 {
		t.Fatalf("package = %s ops %d async %d", p.Name, len(p.Operations), len(p.Async.Operations))
	}
	if _, err := Load(context.Background(), fstest.MapFS{
		"openapi.yaml":  {Data: []byte(miniSpec)},
		"mockmint.yaml": {Data: []byte("async: {}\n")},
	}, "t", Defaults{}, nil); err == nil || !strings.Contains(err.Error(), "no AsyncAPI document") {
		t.Fatalf("async without spec: err = %v", err)
	}
}

func TestAsyncScheduleNeedsMessages(t *testing.T) {
	spec := `asyncapi: 3.0.0
info: {title: t, version: "1"}
channels: {c: {address: x}}
operations: {emit: {action: send, channel: {$ref: "#/channels/c"}}}
`
	_, err := Load(context.Background(), fstest.MapFS{
		"asyncapi.yaml": {Data: []byte(spec)},
		"mockmint.yaml": {Data: []byte("async: {operations: {emit: {schedule: {interval: 1s}}}}\n")},
	}, "t", Defaults{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no messages to publish") {
		t.Fatalf("err = %v", err)
	}
}
