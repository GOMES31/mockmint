package asyncapi

import (
	"errors"
	"math/rand/v2"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func load(t *testing.T, name string) *Spec {
	t.Helper()
	s, err := Load(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func opByID(t *testing.T, s *Spec, id string) *Operation {
	t.Helper()
	for _, o := range s.Operations {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("operation %s not found", id)
	return nil
}

func TestLoadV3(t *testing.T) {
	s := load(t, "orders-v3.yaml")
	if s.Title != "Orders Events" || s.Version != "2.1" || s.AsyncAPI != "3.0.0" {
		t.Fatalf("info = %+v", s)
	}
	if len(s.Warnings) != 0 {
		t.Fatalf("warnings = %v", s.Warnings)
	}

	place := opByID(t, s, "placeOrder")
	if place.Action != ActionReceive || place.Channel.Address != "orders.requests" || place.Channel.AMQP.Is != "queue" || place.Channel.AMQP.Queue.Name != "orders.requests" {
		t.Fatalf("placeOrder = %+v channel %+v", place, place.Channel)
	}
	if len(place.Messages) != 1 || place.Messages[0].ID != "OrderRequest" || len(place.Messages[0].Examples) != 2 {
		t.Fatalf("request messages = %+v", place.Messages)
	}
	big := place.Messages[0].Examples[0]
	if big.Name != "big" || !reflect.DeepEqual(big.Payload, map[string]any{"sku": "ABC-1", "qty": float64(100)}) || big.Headers["x-tenant"] != "acme" {
		t.Fatalf("example = %+v", big)
	}
	if place.Reply == nil || place.Reply.Channel == nil || len(place.Reply.Messages) != 1 || place.Reply.Messages[0].ID != "OrderReply" {
		t.Fatalf("reply = %+v", place.Reply)
	}
	// address: null → unknown; replies go to the request's reply_to.
	if place.Reply.Channel.Address != "" {
		t.Fatalf("reply channel address = %q", place.Reply.Channel.Address)
	}

	pub := opByID(t, s, "publishOrderCreated")
	if pub.Action != ActionSend || pub.Channel.AMQP.Exchange.Name != "orders" || pub.Channel.AMQP.Exchange.Type != "topic" {
		t.Fatalf("publish = %+v", pub.Channel)
	}
	if pub.AMQP.DeliveryMode != 2 || pub.AMQP.Priority != 3 {
		t.Fatalf("operation binding = %+v", pub.AMQP)
	}
	// Omitted operation messages = all channel messages.
	if len(pub.Messages) != 1 || pub.Messages[0].ID != "OrderCreated" || pub.Messages[0].Payload == nil {
		t.Fatalf("publish messages = %+v", pub.Messages)
	}
}

func TestLoadV2(t *testing.T) {
	s := load(t, "users-v2.yaml")
	signup := opByID(t, s, "userSignedUp")
	if signup.Action != ActionSend || signup.Channel.Address != "user.signup" || signup.Channel.AMQP.Exchange.Name != "users" {
		t.Fatalf("signup = %+v %+v", signup, signup.Channel)
	}
	if d := signup.Channel.AMQP.Exchange.Durable; d == nil || *d {
		t.Fatalf("durable = %v", d)
	}
	if signup.AMQP.DeliveryMode != 1 {
		t.Fatalf("binding = %+v", signup.AMQP)
	}
	m := signup.Messages[0]
	if m.ID != "userSignedUp" || m.ContentType != "application/json" || m.AMQP.MessageType != "user.signup" {
		t.Fatalf("traits not applied: %+v", m)
	}
	audit := opByID(t, s, "receiveAudit")
	if audit.Action != ActionReceive || len(audit.Messages) != 2 || audit.Messages[1].Payload != nil {
		t.Fatalf("audit = %+v", audit.Messages)
	}
	joined := strings.Join(s.Warnings, "\n")
	for _, want := range []string{`example "broken" does not match`, `schema format "application/vnd.apache.avro`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}
}

func TestValidate(t *testing.T) {
	s := load(t, "orders-v3.yaml")
	req := opByID(t, s, "placeOrder").Messages[0].Payload
	if v := req.Validate(map[string]any{"sku": "ABC", "qty": float64(2)}); v != nil {
		t.Fatalf("valid payload rejected: %v", v)
	}
	v := req.Validate(map[string]any{"sku": "A", "qty": float64(0)})
	if len(v) != 2 || v[0].Path != "/qty" || v[1].Path != "/sku" {
		t.Fatalf("violations = %+v", v)
	}
	// External file schema with an internal $defs ref; draft-07 asserts formats.
	order := opByID(t, s, "publishOrderCreated").Messages[0].Payload
	v = order.Validate(map[string]any{"id": "x", "items": []any{map[string]any{"qty": float64(1)}}})
	if len(v) != 2 || v[0].Path != "/id" || !strings.Contains(v[0].Message, "uuid") || v[1].Path != "/items/0" || !strings.Contains(v[1].Message, "sku") {
		t.Fatalf("violations = %+v", v)
	}
}

func TestGenerate(t *testing.T) {
	s := load(t, "orders-v3.yaml")
	order := opByID(t, s, "publishOrderCreated").Messages[0].Payload
	for seed := range uint64(100) {
		v, err := order.Generate(rand.New(rand.NewPCG(seed, seed)))
		if err != nil {
			t.Fatal(err)
		}
		if vs := order.Validate(v); vs != nil {
			t.Fatalf("seed %d: generated %v violates schema: %v", seed, v, vs)
		}
	}
	a, _ := order.Generate(rand.New(rand.NewPCG(1, 2)))
	b, _ := order.Generate(rand.New(rand.NewPCG(1, 2)))
	if !reflect.DeepEqual(a, b) {
		t.Fatal("generation is not deterministic")
	}
}

func TestGenerateBudgetAndCycles(t *testing.T) {
	doc := `asyncapi: 3.0.0
info: {title: t, version: "1"}
channels:
  c:
    messages:
      big: {payload: {$ref: "#/components/schemas/l1"}}
      tree: {payload: {$ref: "#/components/schemas/node"}}
operations:
  op: {action: send, channel: {$ref: "#/channels/c"}}
components:
  schemas:
    l1: {type: array, minItems: 100, items: {$ref: "#/components/schemas/l2"}}
    l2: {type: array, minItems: 100, items: {$ref: "#/components/schemas/l3"}}
    l3: {type: array, minItems: 100, items: {type: integer}}
    node:
      type: object
      required: [name]
      properties:
        name: {type: string}
        child: {$ref: "#/components/schemas/node"}
`
	s, err := Load(fstest.MapFS{"a.yaml": {Data: []byte(doc)}}, "a.yaml")
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.Operations[0].Messages
	start := time.Now()
	if _, err := msgs[0].Payload.Generate(rand.New(rand.NewPCG(1, 1))); !errors.Is(err, ErrSchemaTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("budget did not stop generation promptly")
	}
	v, err := msgs[1].Payload.Generate(rand.New(rand.NewPCG(1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(map[string]any)["name"]; !ok {
		t.Fatalf("cyclic schema = %v", v)
	}
}

func TestLoadErrors(t *testing.T) {
	head := "info: {title: t, version: '1'}\n"
	for _, tt := range []struct{ name, doc, want string }{
		{"not asyncapi", "openapi: 3.0.0\n" + head, "no asyncapi field"},
		{"version", "asyncapi: 1.2.0\n" + head, "unsupported AsyncAPI version"},
		{"no operations", "asyncapi: 3.0.0\n" + head + "channels: {}\n", "no operations"},
		{"bad action", "asyncapi: 3.0.0\n" + head + "channels: {c: {}}\noperations: {o: {action: fly, channel: {$ref: '#/channels/c'}}}\n", "want send or receive"},
		{"dangling ref", "asyncapi: 3.0.0\n" + head + "operations: {o: {action: send, channel: {$ref: '#/channels/nope'}}}\n", `no key "channels"`},
		{"escaping ref", "asyncapi: 3.0.0\n" + head + "operations: {o: {action: send, channel: {$ref: '../x.yaml'}}}\n", "escapes the package"},
		{"remote ref", "asyncapi: 3.0.0\n" + head + "operations: {o: {action: send, channel: {$ref: 'https://x/y.yaml'}}}\n", "remote references"},
		{"ref loop", "asyncapi: 3.0.0\n" + head + "channels: {a: {$ref: '#/channels/b'}, b: {$ref: '#/channels/a'}}\noperations: {o: {action: send, channel: {$ref: '#/channels/a'}}}\n", "loop"},
		{"bad binding", "asyncapi: 3.0.0\n" + head + "channels: {c: {bindings: {amqp: {is: topic}}}}\noperations: {o: {action: send, channel: {$ref: '#/channels/c'}}}\n", "want routingKey or queue"},
		{"schema escaping ref", "asyncapi: 3.0.0\n" + head + "channels: {c: {messages: {m: {payload: {$ref: 'x.yaml#/a'}}}}}\noperations: {o: {action: send, channel: {$ref: '#/channels/c'}}}\n", "x.yaml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(fstest.MapFS{"a.yaml": {Data: []byte(tt.doc)}}, "a.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestIsDocument(t *testing.T) {
	if !IsDocument([]byte("asyncapi: 3.0.0\n")) || !IsDocument([]byte(`{"asyncapi":"2.6.0"}`)) || IsDocument([]byte("openapi: 3.1.0\n")) {
		t.Fatal("IsDocument wrong")
	}
}
