//go:build integration

package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/mockmint/mockmint/internal/app"
	"github.com/mockmint/mockmint/internal/config"
)

// TestIntegrationPublishAndReadiness drives the admin API against a real
// RabbitMQ: readiness follows the broker connection, and the publish
// endpoint delivers a rendered example to the channel's exchange.
func TestIntegrationPublishAndReadiness(t *testing.T) {
	ctx := context.Background()
	c, err := rabbitmq.Run(ctx, "docker.io/library/rabbitmq:4-management-alpine")
	if err != nil {
		t.Fatalf("start RabbitMQ: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	url, err := c.AmqpURL(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Packages.Paths = []string{"../../examples/orders-events"}
	cfg.AMQP.URL = url
	cfg.AMQP.ReconnectMin = config.Duration(100 * time.Millisecond)
	a, err := app.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(a, Options{MaxUploadBytes: 1 << 20}))
	defer srv.Close()

	deadline := time.Now().Add(30 * time.Second)
	for call(t, srv, "GET", "/readyz", "", nil, false).status != 200 {
		if time.Now().After(deadline) {
			t.Fatal("never became ready")
		}
		time.Sleep(50 * time.Millisecond)
	}

	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.QueueBind(q.Name, "order.created", "orders.events", false, nil); err != nil {
		t.Fatal(err)
	}

	r := call(t, srv, "POST", "/admin/async/orders-events/order.created/publish", "application/json", []byte(`{"example":"gadget"}`), false)
	if r.status != 202 {
		t.Fatalf("publish = %d %s", r.status, r.body)
	}
	var got map[string]any
	deadline = time.Now().Add(10 * time.Second)
	for got == nil {
		// The scheduled publisher also sends every 5s; wait for gadget.
		if d, ok, _ := ch.Get(q.Name, true); ok {
			var m map[string]any
			if json.Unmarshal(d.Body, &m) == nil && m["sku"] == "GADGET-1" {
				got = m
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("published message not received")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got["createdAt"] != "2025-01-01T12:00:00.000Z" {
		t.Fatalf("message = %v", got)
	}

	// Traffic and metrics saw it.
	if tr := call(t, srv, "GET", "/admin/traffic?protocol=amqp", "", nil, false); !json.Valid([]byte(tr.body)) || len(tr.body) < 10 {
		t.Fatalf("amqp traffic = %s", tr.body)
	}
	if a.Metrics.AMQPPublish.Value("orders-events", "publishOrderCreated", "ok") == 0 {
		t.Fatal("publish metric not incremented")
	}

	// Broker gone → not ready.
	if err := c.Stop(ctx, nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second)
	for call(t, srv, "GET", "/readyz", "", nil, false).status != 503 {
		if time.Now().After(deadline) {
			t.Fatal("readiness did not follow the broker")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
