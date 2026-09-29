//go:build integration

package amqp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/pkg"
)

// Integration tests run against a real RabbitMQ in a container:
//
//	go test -tags integration ./internal/protocol/amqp/
//
// They need Docker, or Podman with its Docker-compatible API socket.

const rabbitImage = "docker.io/library/rabbitmq:4-management-alpine"

type broker struct {
	c   *rabbitmq.RabbitMQContainer
	url string
}

func startBroker(t *testing.T) *broker {
	t.Helper()
	ctx := context.Background()
	c, err := rabbitmq.Run(ctx, rabbitImage)
	if err != nil {
		t.Fatalf("start RabbitMQ container: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	url, err := c.AmqpURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &broker{c: c, url: url}
}

// startEngine runs an engine over the orders-events example until the test
// ends. tweak can adjust the loaded package (e.g. faster schedules).
func startEngine(t *testing.T, b *broker, tweak func(*pkg.Package)) *Engine {
	t.Helper()
	p := loadPkg(t)
	if tweak != nil {
		tweak(p)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	e, err := New([]*pkg.Package{p}, Options{
		URL: b.url, Prefetch: 10, Heartbeat: 5 * time.Second,
		ReconnectMin: 100 * time.Millisecond, ReconnectMax: time.Second, ConfirmTimeout: 5 * time.Second,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop")
		}
	})
	waitFor(t, "engine connected", 30*time.Second, e.Connected)
	return e
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// client is a test-side connection with an exclusive reply queue.
type client struct {
	conn    *amqp.Connection
	ch      *amqp.Channel
	replyQ  string
	replies <-chan amqp.Delivery
}

func dial(t *testing.T, url string) *client {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	replies, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &client{conn: conn, ch: ch, replyQ: q.Name, replies: replies}
}

func (c *client) request(t *testing.T, body, corr string) amqp.Delivery {
	t.Helper()
	err := c.ch.PublishWithContext(context.Background(), "", "orders.requests", false, false, amqp.Publishing{
		ContentType: "application/json", CorrelationId: corr, ReplyTo: c.replyQ, Body: []byte(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-c.replies:
		return d
	case <-time.After(10 * time.Second):
		t.Fatalf("no reply to %s", body)
		return amqp.Delivery{}
	}
}

// get polls queue for one message.
func (c *client) get(t *testing.T, queue string) amqp.Delivery {
	t.Helper()
	var d amqp.Delivery
	waitFor(t, "a message on "+queue, 10*time.Second, func() bool {
		var ok bool
		var err error
		d, ok, err = c.ch.Get(queue, true)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	})
	return d
}

func jsonBody(t *testing.T, d amqp.Delivery) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(d.Body, &m); err != nil {
		t.Fatalf("body %s: %v", d.Body, err)
	}
	return m
}

func TestIntegration(t *testing.T) {
	b := startBroker(t)
	e := startEngine(t, b, func(p *pkg.Package) {
		for _, op := range p.Async.Operations {
			if op.Schedule != nil {
				op.Schedule.Interval = config.Duration(150 * time.Millisecond)
			}
		}
	})
	c := dial(t, b.url)

	t.Run("request reply", func(t *testing.T) {
		d := c.request(t, `{"sku":"WIDGET-9","qty":500}`, "corr-bulk")
		if d.CorrelationId != "corr-bulk" || jsonBody(t, d)["status"] != "rejected" {
			t.Fatalf("bulk reply = %s corr %q", d.Body, d.CorrelationId)
		}
		d = c.request(t, `{"sku":"WIDGET-9","qty":1}`, "corr-single")
		m := jsonBody(t, d)
		if d.CorrelationId != "corr-single" || m["status"] != "accepted" || m["sku"] != "WIDGET-9" || !uuidRE.MatchString(fmt.Sprint(d.Headers["x-order-id"])) {
			t.Fatalf("single reply = %s headers %v", d.Body, d.Headers)
		}
		if d.ContentType != "application/json" || d.MessageId == "" {
			t.Fatalf("reply properties = %+v", d)
		}
	})

	t.Run("invalid request is dead-lettered", func(t *testing.T) {
		err := c.ch.PublishWithContext(context.Background(), "", "orders.requests", false, false, amqp.Publishing{
			ContentType: "application/json", ReplyTo: c.replyQ, Body: []byte(`{"sku":"X","qty":0}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		d := c.get(t, "orders.dlx.queue")
		if string(d.Body) != `{"sku":"X","qty":0}` {
			t.Fatalf("dead-lettered body = %s", d.Body)
		}
		if deaths, _ := d.Headers["x-death"].([]any); len(deaths) == 0 {
			t.Fatalf("missing x-death header: %v", d.Headers)
		}
		select {
		case r := <-c.replies:
			t.Fatalf("invalid request got a reply: %s", r.Body)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("sink acks valid and dead-letters invalid", func(t *testing.T) {
		pub := func(body string) {
			if err := c.ch.PublishWithContext(context.Background(), "payments.events", "payment.settled", false, false,
				amqp.Publishing{ContentType: "application/json", Body: []byte(body)}); err != nil {
				t.Fatal(err)
			}
		}
		pub(`{"orderId":"o-1","amount":10}`)
		pub(`{"orderId":"o-2","amount":-5}`)
		if d := c.get(t, "orders.dlx.queue"); !strings.Contains(string(d.Body), "o-2") {
			t.Fatalf("dead-lettered = %s", d.Body)
		}
		waitFor(t, "payments queue drained", 5*time.Second, func() bool {
			q, err := c.ch.QueueDeclarePassive("orders.payments", true, false, false, false,
				amqp.Table{"x-dead-letter-exchange": "orders.dlx"})
			return err == nil && q.Messages == 0
		})
	})

	t.Run("scheduled and on-demand publish", func(t *testing.T) {
		q, err := c.ch.QueueDeclare("", false, true, true, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ch.QueueBind(q.Name, "order.created", "orders.events", false, nil); err != nil {
			t.Fatal(err)
		}
		skus := map[string]bool{}
		waitFor(t, "both scheduled examples", 10*time.Second, func() bool {
			if d, ok, _ := c.ch.Get(q.Name, true); ok {
				skus[jsonBody(t, d)["sku"].(string)] = true
				if d.DeliveryMode != amqp.Persistent {
					t.Fatalf("delivery mode = %d", d.DeliveryMode)
				}
			}
			return skus["WIDGET-9"] && skus["GADGET-1"]
		})
		if err := e.Publish(context.Background(), "orders-events", "publishOrderCreated", "gadget"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unanswerable requests are dead-lettered", func(t *testing.T) {
		for _, tc := range []struct{ name, replyTo, marker string }{
			{"unroutable reply_to", "no.such.queue", "WIDGET-UNROUTABLE"},
			{"missing reply_to", "", "WIDGET-NOREPLYTO"},
		} {
			body := `{"sku":"` + tc.marker + `","qty":1}`
			if err := c.ch.PublishWithContext(context.Background(), "", "orders.requests", false, false, amqp.Publishing{
				ContentType: "application/json", ReplyTo: tc.replyTo, Body: []byte(body),
			}); err != nil {
				t.Fatal(err)
			}
			if d := c.get(t, "orders.dlx.queue"); string(d.Body) != body {
				t.Fatalf("%s: dead-lettered %s, want %s", tc.name, d.Body, body)
			}
		}
	})

	t.Run("publisher channel recovers after a channel exception", func(t *testing.T) {
		// Deleting the exchange makes the next publish fail with a 404
		// channel exception, which closes mockmint's publisher channel. The
		// session must restart, redeclare the exchange and publish again.
		if err := c.ch.ExchangeDelete("orders.events", false, false); err != nil {
			t.Fatal(err)
		}
		_ = e.Publish(context.Background(), "orders-events", "publishOrderCreated", "widget")
		waitFor(t, "publishing works again", 30*time.Second, func() bool {
			return e.Publish(context.Background(), "orders-events", "publishOrderCreated", "widget") == nil
		})
	})

	t.Run("reconnects after the broker drops connections", func(t *testing.T) {
		code, out, err := b.c.Exec(context.Background(), []string{"rabbitmqctl", "close_all_connections", "mockmint test"})
		if err != nil || code != 0 {
			msg, _ := io.ReadAll(out)
			t.Fatalf("close_all_connections: code %d err %v %s", code, err, msg)
		}
		waitFor(t, "engine disconnect", 10*time.Second, func() bool { return !e.Connected() })
		waitFor(t, "engine reconnect", 30*time.Second, e.Connected)
		c2 := dial(t, b.url) // the old client connection was closed too
		for i := range 3 {
			corr := fmt.Sprintf("after-reconnect-%d", i)
			if d := c2.request(t, `{"sku":"WIDGET-9","qty":500}`, corr); d.CorrelationId != corr {
				t.Fatalf("reply after reconnect = %+v", d)
			}
		}
	})
}

func TestIntegrationBrokerDownAtStartup(t *testing.T) {
	p := loadPkg(t)
	e, err := New([]*pkg.Package{p}, Options{
		URL: "amqp://guest:guest@127.0.0.1:1/", Prefetch: 1,
		ReconnectMin: 50 * time.Millisecond, ReconnectMax: 100 * time.Millisecond, ConfirmTimeout: time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	e.Run(ctx)
	if time.Since(start) > 2*time.Second || e.Connected() {
		t.Fatal("engine must keep retrying quietly and stop with its context")
	}
}
