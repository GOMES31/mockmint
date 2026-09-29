package amqp

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/dispatch"
	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/spec/asyncapi"
	"github.com/mockmint/mockmint/internal/template"
)

// Delivery is the part of an incoming message the handler looks at.
type Delivery struct {
	Exchange      string
	RoutingKey    string
	Body          []byte
	ContentType   string
	CorrelationID string
	ReplyTo       string
	MessageID     string
	Headers       amqp.Table
}

// Verdict is what happens to the incoming message.
type Verdict int

const (
	// Ack acknowledges the message (after the reply, if any, is confirmed).
	Ack Verdict = iota
	// Reject nacks without requeue: the broker dead-letters it when the
	// queue has a DLX, otherwise drops it.
	Reject
)

// Outcome is the handler's decision for one delivery.
type Outcome struct {
	Verdict  Verdict
	Reason   string        // why, for logs; set for rejects and dropped replies
	Delay    time.Duration // injected latency before acting
	Reply    *Outgoing     // nil: no reply
	Example  string        // reply example used
	Warnings []string      // validation findings in warn mode
}

// Outgoing is a message to publish.
type Outgoing struct {
	Exchange   string
	RoutingKey string
	Mandatory  bool
	Msg        amqp.Publishing
}

// Handle decides what to do with d for a receive operation.
func Handle(p *pkg.Package, op *pkg.AsyncOperation, d Delivery) Outcome {
	rng := template.NewRand(p.Seed, []byte(op.ID), []byte(d.RoutingKey), d.Body)
	out := Outcome{}
	doc, isJSON := decodeJSON(d.Body)

	if op.Validation != pkg.ValidationOff {
		if problems := validateIncoming(op.Messages, doc, isJSON, headerDoc(d.Headers)); problems != "" {
			if op.Validation == pkg.ValidationStrict {
				return Outcome{Verdict: Reject, Reason: "invalid message: " + problems}
			}
			out.Warnings = append(out.Warnings, "invalid message: "+problems)
		}
	}

	fault, faulted := op.Behavior.Fault(rng)
	out.Delay = op.Behavior.Delay(rng)
	if faulted && fault.Action == behavior.ActionDeadLetter {
		out.Verdict, out.Reason = Reject, "deadletter fault injected"
		return out
	}

	rs := op.Replies
	if rs == nil {
		return out // sink: ack
	}

	dreq := &dispatch.Request{Path: d.RoutingKey, Header: toHeader(d.Headers), Body: d.Body}
	name, ok, err := rs.Dispatcher.Dispatch(dreq)
	if err != nil {
		out.Verdict, out.Reason = Reject, "dispatch failed: "+err.Error()
		return out
	}
	mt := rs.Templates[name]
	if !ok || mt == nil {
		mt = rs.Fallback
	}
	if mt == nil {
		out.Verdict, out.Reason = Reject, "no reply example matches"
		return out
	}
	out.Example = mt.Example

	if faulted && fault.Action == behavior.ActionDrop {
		out.Reason = "drop fault injected: reply not sent"
		return out
	}

	exchange, key := "", d.ReplyTo
	if key == "" {
		exchange, key = rs.Exchange, rs.RoutingKey
	}
	if key == "" && exchange == "" {
		out.Verdict, out.Reason = Reject, "request has no reply_to and the operation has no fixed reply address"
		return out
	}

	req := templateRequest(d, doc, isJSON)
	msg, err := render(p, op.ID, mt, req, rng)
	if err != nil {
		out.Verdict, out.Reason = Reject, err.Error()
		return out
	}
	if op.Validation != pkg.ValidationOff {
		if problems := validateOutgoing(mt.Message, msg.Body); problems != "" {
			if op.Validation == pkg.ValidationStrict {
				out.Verdict, out.Reason = Reject, fmt.Sprintf("reply example %q is invalid: %s", mt.Example, problems)
				return out
			}
			out.Warnings = append(out.Warnings, fmt.Sprintf("reply example %q is invalid: %s", mt.Example, problems))
		}
	}
	msg.CorrelationId = d.CorrelationID
	if msg.CorrelationId == "" {
		msg.CorrelationId = d.MessageID // RPC convention when the request has no correlation id
	}
	applyBinding(&msg, rs.Binding, p)
	// Mandatory: an unroutable reply (e.g. the caller's reply queue is gone)
	// comes back instead of being confirmed and dropped.
	out.Reply = &Outgoing{Exchange: exchange, RoutingKey: key, Mandatory: true, Msg: msg}
	return out
}

// Render renders example of a send operation. seq distinguishes successive
// publishes of the same example so seeded runs still vary between them.
func Render(p *pkg.Package, op *pkg.AsyncOperation, example string, seq uint64) (*Outgoing, error) {
	mt := op.Templates[example]
	if mt == nil {
		return nil, fmt.Errorf("operation %s has no example %q (have %s)", op.ID, example, strings.Join(pkg.ExampleNames(op.Templates), ", "))
	}
	rng := template.NewRand(p.Seed, []byte(op.ID), []byte(example), []byte(strconv.FormatUint(seq, 10)))
	msg, err := render(p, op.ID, mt, template.Request{Path: op.RoutingKey}, rng)
	if err != nil {
		return nil, err
	}
	if op.Validation != pkg.ValidationOff {
		if problems := validateOutgoing(mt.Message, msg.Body); problems != "" && op.Validation == pkg.ValidationStrict {
			return nil, fmt.Errorf("example %q is invalid: %s", example, problems)
		}
	}
	applyBinding(&msg, op.AMQP, p)
	return &Outgoing{Exchange: op.Exchange, RoutingKey: op.RoutingKey, Mandatory: op.AMQP.Mandatory, Msg: msg}, nil
}

func render(p *pkg.Package, opID string, mt *pkg.MessageTemplate, req template.Request, rng *rand.Rand) (amqp.Publishing, error) {
	msg := amqp.Publishing{ContentType: mt.ContentType, Body: mt.Body, Type: mt.Message.AMQP.MessageType, ContentEncoding: mt.Message.AMQP.ContentEncoding}
	var data *template.Data
	if mt.BodyTemplate != nil || mt.HeaderTemplates != nil {
		data = template.NewData(req, opID, mt.Example, rng, p.Now())
	}
	if mt.BodyTemplate != nil {
		b, err := mt.BodyTemplate.Execute(data)
		if err != nil {
			return msg, fmt.Errorf("render example %q: %w", mt.Example, err)
		}
		msg.Body = b
	}
	if len(mt.Headers) > 0 || len(mt.HeaderTemplates) > 0 {
		msg.Headers = amqp.Table{}
		for k, v := range mt.Headers {
			msg.Headers[k] = tableValue(v)
		}
		for _, ht := range mt.HeaderTemplates { // sorted: fixed RNG order
			b, err := ht.Template.Execute(data)
			if err != nil {
				return msg, fmt.Errorf("render example %q header %s: %w", mt.Example, ht.Name, err)
			}
			msg.Headers[ht.Name] = string(b)
		}
		if err := msg.Headers.Validate(); err != nil {
			return msg, fmt.Errorf("example %q headers: %w", mt.Example, err)
		}
	}
	msg.MessageId = uuid(rng)
	return msg, nil
}

func applyBinding(msg *amqp.Publishing, b asyncapi.OperationBinding, p *pkg.Package) {
	msg.DeliveryMode = b.DeliveryMode
	msg.Priority = b.Priority
	msg.UserId = b.UserID
	if b.Expiration > 0 {
		msg.Expiration = strconv.Itoa(b.Expiration)
	}
	if b.Timestamp {
		msg.Timestamp = p.Now()
	}
	if len(b.CC) > 0 {
		if msg.Headers == nil {
			msg.Headers = amqp.Table{}
		}
		cc := make([]any, len(b.CC))
		for i, k := range b.CC {
			cc[i] = k
		}
		msg.Headers["CC"] = cc
	}
}

// validateIncoming checks the body and headers against the operation's
// messages; the message is valid when one of them accepts both (AsyncAPI
// oneOf semantics). It returns the first message's problems otherwise.
func validateIncoming(msgs []*asyncapi.Message, doc any, isJSON bool, headers map[string]any) string {
	var first string
	for _, m := range msgs {
		problems := messageProblems(m, doc, isJSON, headers)
		if problems == "" {
			return ""
		}
		if first == "" {
			first = problems
		}
	}
	return first
}

func messageProblems(m *asyncapi.Message, doc any, isJSON bool, headers map[string]any) string {
	var parts []string
	if m.Payload != nil {
		if !isJSON {
			parts = append(parts, "body is not JSON")
		} else if v := m.Payload.Validate(doc); v != nil {
			parts = append(parts, joinViolations(v))
		}
	}
	if m.Headers != nil {
		if v := m.Headers.Validate(headers); v != nil {
			parts = append(parts, "headers: "+joinViolations(v))
		}
	}
	return strings.Join(parts, "; ")
}

// headerDoc converts AMQP headers to the JSON value model for validation.
func headerDoc(t amqp.Table) map[string]any {
	out := make(map[string]any, len(t))
	for k, v := range t {
		out[k] = jsonValue(v)
	}
	return out
}

func jsonValue(v any) any {
	switch t := v.(type) {
	case amqp.Table:
		return headerDoc(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonValue(x)
		}
		return out
	case int8:
		return float64(t)
	case int16:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint8:
		return float64(t)
	case uint16:
		return float64(t)
	case uint32:
		return float64(t)
	case float32:
		return float64(t)
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case amqp.Decimal:
		return float64(t.Value) / math.Pow10(int(t.Scale))
	default:
		return v
	}
}

func validateOutgoing(m *asyncapi.Message, body []byte) string {
	if m == nil || m.Payload == nil {
		return ""
	}
	doc, ok := decodeJSON(body)
	if !ok {
		return "body is not JSON"
	}
	return joinViolations(m.Payload.Validate(doc))
}

func joinViolations(v []asyncapi.Violation) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = x.String()
	}
	return strings.Join(parts, "; ")
}

func decodeJSON(b []byte) (any, bool) {
	var v any
	if len(b) == 0 || json.Unmarshal(b, &v) != nil {
		return nil, false
	}
	return v, true
}

// toHeader exposes AMQP headers to dispatchers as canonical HTTP-style
// headers with string values.
func toHeader(t amqp.Table) http.Header {
	h := http.Header{}
	for k, v := range t {
		h.Set(k, headerString(v))
	}
	return h
}

func templateRequest(d Delivery, doc any, isJSON bool) template.Request {
	r := template.Request{Path: d.RoutingKey, Headers: map[string]string{}, RawBody: string(d.Body)}
	for k, v := range d.Headers {
		r.Headers[http.CanonicalHeaderKey(k)] = headerString(v)
	}
	// Message properties are exposed next to headers.
	for k, v := range map[string]string{
		"Correlation-Id": d.CorrelationID, "Reply-To": d.ReplyTo, "Message-Id": d.MessageID,
		"Content-Type": d.ContentType, "Exchange": d.Exchange, "Routing-Key": d.RoutingKey,
	} {
		if v != "" {
			r.Headers[k] = v
		}
	}
	if isJSON {
		r.Body = doc
	}
	return r
}

func headerString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// tableValue converts JSON-model values to types amqp.Table accepts.
func tableValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := amqp.Table{}
		for k, val := range t {
			out[k] = tableValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = tableValue(val)
		}
		return out
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	default:
		return v
	}
}

func uuid(rng *rand.Rand) string {
	d := template.NewData(template.Request{}, "", "", rng, time.Time{})
	return d.UUID()
}
