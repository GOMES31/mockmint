package pkg

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/config"
	"github.com/mockmint/mockmint/internal/dispatch"
	"github.com/mockmint/mockmint/internal/spec/asyncapi"
	"github.com/mockmint/mockmint/internal/template"
)

// AsyncManifest is the async section of mockmint.yaml.
type AsyncManifest struct {
	// Declare exchanges, queues and bindings on connect (default true).
	Declare *bool `yaml:"declare"`
	// Validation is strict, warn or off (default: the package validation).
	Validation string `yaml:"validation"`
	// Behavior is the default for every async operation. Only latency and
	// drop/deadletter faults apply to messages.
	Behavior behavior.Config `yaml:"behavior"`
	// DeadLetter is where rejected messages of consumed queues go.
	DeadLetter *DeadLetter `yaml:"deadLetter"`
	// Operations are keyed by AsyncAPI operation id.
	Operations map[string]AsyncOverrides `yaml:"operations"`
}

// DeadLetter names the dead-letter exchange (declared as fanout) and the
// queue bound to it (default: "<exchange>.queue").
type DeadLetter struct {
	Exchange string `yaml:"exchange"`
	Queue    string `yaml:"queue"`
}

// AsyncOverrides configure one AsyncAPI operation.
type AsyncOverrides struct {
	// Queue overrides the queue a receive operation consumes.
	Queue string `yaml:"queue"`
	// Exchange and RoutingKey override where a send operation publishes.
	Exchange   *string `yaml:"exchange"`
	RoutingKey string  `yaml:"routingKey"`
	// ReplyWith names a send operation whose messages are the replies of
	// this receive operation (for AsyncAPI 2.x, which has no reply object;
	// it overrides a 3.0 reply).
	ReplyWith string `yaml:"replyWith"`
	// NoReply turns a 3.0 request/reply operation into a sink.
	NoReply bool `yaml:"noReply"`

	Dispatcher dispatch.Config `yaml:"dispatcher"`
	Behavior   behavior.Config `yaml:"behavior"`
	// Fallback is the reply example used when no example matches; without
	// one the request is dead-lettered.
	Fallback   string    `yaml:"fallback"`
	Validation string    `yaml:"validation"`
	Schedule   *Schedule `yaml:"schedule"`
}

// Schedule publishes a send operation's examples periodically, in order,
// cycling.
type Schedule struct {
	Interval     config.Duration `yaml:"interval"`
	InitialDelay config.Duration `yaml:"initialDelay"`
	Examples     []string        `yaml:"examples"` // default: all, by name
}

// Async is a package's compiled AsyncAPI side.
type Async struct {
	Declare    bool
	DeadLetter *DeadLetter
	Operations []*AsyncOperation
}

// AsyncOperation is an AsyncAPI operation with everything needed to mock it.
type AsyncOperation struct {
	*asyncapi.Operation
	Validation string
	Behavior   *behavior.Behavior

	// Receive operations: the queue to consume and, when it is bound to an
	// exchange, the binding. Replies is nil for sinks.
	Queue        string
	BindExchange string
	BindKey      string
	Replies      *ReplySet

	// Send operations: publish target, examples and optional schedule.
	// ExchangeOverride is set when mockmint.yaml replaced the channel's
	// exchange; that exchange must already exist.
	ExchangeOverride bool
	Exchange         string
	RoutingKey       string
	Templates        map[string]*MessageTemplate
	Schedule         *Schedule
}

// ReplySet is how a receive operation answers.
type ReplySet struct {
	Messages   []*asyncapi.Message // for validating replies
	Channel    *asyncapi.Channel   // the reply channel; nil when replies only go to reply_to
	Exchange   string              // fixed destination when the request has no reply_to
	RoutingKey string              // "" with Exchange "": no fixed destination
	Binding    asyncapi.OperationBinding
	Templates  map[string]*MessageTemplate
	Dispatcher dispatch.Dispatcher
	Fallback   *MessageTemplate
}

// MessageTemplate is a compiled message example.
type MessageTemplate struct {
	Example         string
	Message         *asyncapi.Message
	ContentType     string
	Body            []byte
	BodyTemplate    *template.Template
	Headers         map[string]any // static header values
	HeaderTemplates []HeaderTemplate
}

// ExampleNames returns the template names sorted.
func ExampleNames(t map[string]*MessageTemplate) []string {
	names := make([]string, 0, len(t))
	for n := range t {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func compileAsync(pk *Package, m *AsyncManifest, validation, templating string) (*Async, error) {
	if m == nil {
		m = &AsyncManifest{}
	}
	a := &Async{Declare: m.Declare == nil || *m.Declare, DeadLetter: m.DeadLetter}
	if dl := a.DeadLetter; dl != nil {
		if dl.Exchange == "" {
			return nil, errors.New("deadLetter.exchange is required")
		}
		dl.Queue = cmp.Or(dl.Queue, dl.Exchange+".queue")
	}
	asyncValidation := cmp.Or(m.Validation, validation)
	if err := checkValidation(asyncValidation); err != nil {
		return nil, err
	}
	ops := map[string]*asyncapi.Operation{}
	for _, op := range pk.AsyncSpec.Operations {
		ops[op.ID] = op
	}
	for id := range m.Operations {
		if ops[id] == nil {
			return nil, fmt.Errorf("operations: %q matches no AsyncAPI operation", id)
		}
	}
	var errs []error
	for _, op := range pk.AsyncSpec.Operations {
		ov := m.Operations[op.ID]
		ao, err := compileAsyncOperation(pk, m, op, ov, ops, cmp.Or(ov.Validation, asyncValidation), templating)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", op.ID, err))
			continue
		}
		a.Operations = append(a.Operations, ao)
	}
	return a, errors.Join(errs...)
}

func compileAsyncOperation(pk *Package, m *AsyncManifest, op *asyncapi.Operation, ov AsyncOverrides,
	ops map[string]*asyncapi.Operation, validation, templating string) (*AsyncOperation, error) {
	if err := checkValidation(validation); err != nil {
		return nil, err
	}
	ao := &AsyncOperation{Operation: op, Validation: validation}
	bcfg := behavior.Merge(m.Behavior, ov.Behavior)
	if bcfg.RateLimit != nil {
		return nil, errors.New("behavior: rateLimit applies to HTTP operations only")
	}
	for i, f := range bcfg.Faults {
		if f.Action == "" || f.Action == behavior.ActionStatus {
			return nil, fmt.Errorf("behavior: faults[%d]: messages support action drop or deadletter", i)
		}
	}
	var err error
	if ao.Behavior, err = behavior.Compile(bcfg, nil); err != nil {
		return nil, fmt.Errorf("behavior: %w", err)
	}
	rng := template.NewRand(cmp.Or(pk.Seed, 1), []byte("async"), []byte(op.ID))

	if op.Action == asyncapi.ActionSend {
		if ov.Queue != "" || ov.ReplyWith != "" || ov.NoReply || ov.Fallback != "" || ov.Dispatcher.Type != "" {
			return nil, errors.New("queue, replyWith, noReply, fallback and dispatcher apply to receive operations")
		}
		ao.Exchange, ao.RoutingKey = publishTarget(op.Channel)
		if ov.Exchange != nil && *ov.Exchange != ao.Exchange {
			ao.Exchange, ao.ExchangeOverride = *ov.Exchange, true
		}
		ao.RoutingKey = cmp.Or(ov.RoutingKey, ao.RoutingKey)
		if ao.Templates, err = compileMessages(op.ID, op.Messages, templating, rng); err != nil {
			return nil, err
		}
		if s := ov.Schedule; s != nil {
			if len(ao.Templates) == 0 {
				return nil, errors.New("schedule: the operation has no messages to publish")
			}
			if s.Interval.D() < 10*time.Millisecond {
				return nil, errors.New("schedule.interval must be at least 10ms")
			}
			if len(s.Examples) == 0 {
				s.Examples = ExampleNames(ao.Templates)
			}
			for _, n := range s.Examples {
				if ao.Templates[n] == nil {
					return nil, fmt.Errorf("schedule: unknown example %q", n)
				}
			}
			ao.Schedule = s
		}
		return ao, nil
	}

	// Receive.
	if ov.Exchange != nil || ov.RoutingKey != "" || ov.Schedule != nil {
		return nil, errors.New("exchange, routingKey and schedule apply to send operations")
	}
	ao.Queue, ao.BindExchange, ao.BindKey = consumeTarget(pk.Name, op)
	ao.Queue = cmp.Or(ov.Queue, ao.Queue)

	var replyMsgs []*asyncapi.Message
	var replyChannel *asyncapi.Channel
	var replyBinding asyncapi.OperationBinding
	switch {
	case ov.NoReply:
	case ov.ReplyWith != "":
		target := ops[ov.ReplyWith]
		if target == nil || target.Action != asyncapi.ActionSend {
			return nil, fmt.Errorf("replyWith %q must name a send operation", ov.ReplyWith)
		}
		replyMsgs, replyChannel, replyBinding = target.Messages, target.Channel, target.AMQP
	case op.Reply != nil:
		replyMsgs, replyChannel = op.Reply.Messages, op.Reply.Channel
	}
	if replyMsgs == nil {
		if ov.Dispatcher.Type != "" || ov.Fallback != "" {
			return nil, errors.New("dispatcher and fallback need a reply (reply in the spec, or replyWith)")
		}
		return ao, nil // sink
	}

	rs := &ReplySet{Messages: replyMsgs, Binding: replyBinding}
	if replyChannel != nil && replyChannel.Address != "" {
		rs.Channel = replyChannel
		rs.Exchange, rs.RoutingKey = publishTarget(replyChannel)
	}
	if rs.Templates, err = compileMessages(op.ID+" reply", replyMsgs, templating, rng); err != nil {
		return nil, err
	}
	names := ExampleNames(rs.Templates)
	cands := requestCandidates(op.Messages, rs.Templates)
	if rs.Dispatcher, err = dispatch.Build(ov.Dispatcher, dispatch.Inputs{
		Examples: names, DefaultExample: defaultReply(names, cands), Candidates: cands,
	}); err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	if ov.Fallback != "" {
		if rs.Fallback = rs.Templates[ov.Fallback]; rs.Fallback == nil {
			return nil, fmt.Errorf("fallback: unknown example %q", ov.Fallback)
		}
	}
	ao.Replies = rs
	return ao, nil
}

// publishTarget is where messages for ch are published: the exchange with
// the address as routing key, or the default exchange with the queue name.
func publishTarget(ch *asyncapi.Channel) (exchange, key string) {
	b := ch.AMQP
	if b.Is == "queue" {
		return "", cmp.Or(b.Queue.Name, ch.Address)
	}
	return exchangeName(b.Exchange), cmp.Or(ch.Address, b.Queue.Name)
}

// consumeTarget is the queue a receive operation consumes. A routingKey
// channel without a named queue gets a mockmint-owned queue bound to its
// exchange.
func consumeTarget(pkgName string, op *asyncapi.Operation) (queue, bindExchange, bindKey string) {
	ch := op.Channel
	b := ch.AMQP
	if b.Is == "queue" {
		return cmp.Or(b.Queue.Name, ch.Address), "", ""
	}
	queue = cmp.Or(b.Queue.Name, "mockmint."+pkgName+"."+op.ID)
	return queue, exchangeName(b.Exchange), cmp.Or(ch.Address, "#")
}

// exchangeName maps AsyncAPI's "default" exchange type/name to "".
func exchangeName(e asyncapi.Exchange) string {
	if e.Type == "default" || e.Name == "default" {
		return ""
	}
	return e.Name
}

func compileMessages(owner string, msgs []*asyncapi.Message, templating string, rng *rand.Rand) (map[string]*MessageTemplate, error) {
	out := map[string]*MessageTemplate{}
	for _, msg := range msgs {
		for _, ex := range msg.Examples {
			if prev, dup := out[ex.Name]; dup {
				return nil, fmt.Errorf("example %q is defined by messages %s and %s", ex.Name, prev.Message.ID, msg.ID)
			}
			mt, err := compileMessage(owner, msg, ex, templating)
			if err != nil {
				return nil, err
			}
			out[ex.Name] = mt
		}
	}
	if len(out) > 0 || len(msgs) == 0 {
		return out, nil
	}
	// No examples at all: synthesize one from the first message's payload.
	msg := msgs[0]
	ex := asyncapi.Example{Name: "generated", Payload: map[string]any{}, HasPayload: true}
	if msg.Payload != nil {
		v, err := msg.Payload.Generate(rng)
		if err != nil {
			return nil, fmt.Errorf("message %s: %w", msg.ID, err)
		}
		ex.Payload = v
	}
	mt, err := compileMessage(owner, msg, ex, templating)
	if err != nil {
		return nil, err
	}
	out["generated"] = mt
	return out, nil
}

func compileMessage(owner string, msg *asyncapi.Message, ex asyncapi.Example, templating string) (*MessageTemplate, error) {
	mt := &MessageTemplate{Example: ex.Name, Message: msg, ContentType: cmp.Or(msg.ContentType, "application/json")}
	if ex.HasPayload {
		if s, ok := ex.Payload.(string); ok && !strings.Contains(mt.ContentType, "json") {
			mt.Body = []byte(s)
		} else {
			b, err := json.Marshal(ex.Payload)
			if err != nil {
				return nil, fmt.Errorf("example %q: %w", ex.Name, err)
			}
			mt.Body = b
		}
	}
	render := func(s string) bool {
		return templating == TemplatingOn || (templating == TemplatingAuto && template.IsTemplate(s))
	}
	if render(string(mt.Body)) {
		t, err := template.Parse(owner+" "+ex.Name, string(mt.Body))
		if err != nil {
			return nil, fmt.Errorf("example %q payload template: %w", ex.Name, err)
		}
		mt.BodyTemplate = t
	}
	for _, k := range sortedKeys(ex.Headers) {
		v := ex.Headers[k]
		if s, ok := v.(string); ok && render(s) {
			t, err := template.Parse(owner+" "+ex.Name+" "+k, s)
			if err != nil {
				return nil, fmt.Errorf("example %q header %s template: %w", ex.Name, k, err)
			}
			mt.HeaderTemplates = append(mt.HeaderTemplates, HeaderTemplate{Name: k, Template: t})
			continue
		}
		if mt.Headers == nil {
			mt.Headers = map[string]any{}
		}
		mt.Headers[k] = v
	}
	return mt, nil
}

// requestCandidates pairs request message examples with reply examples of
// the same name, for the auto dispatcher.
func requestCandidates(req []*asyncapi.Message, replies map[string]*MessageTemplate) []dispatch.Candidate {
	byName := map[string]dispatch.Candidate{}
	for _, msg := range req {
		for _, ex := range msg.Examples {
			if replies[ex.Name] == nil {
				continue
			}
			c := dispatch.Candidate{Name: ex.Name}
			if ex.HasPayload {
				c.Body = ex.Payload
			}
			for k, v := range ex.Headers {
				if c.Headers == nil {
					c.Headers = map[string]string{}
				}
				c.Headers[http.CanonicalHeaderKey(k)] = scalar(v)
			}
			byName[ex.Name] = c
		}
	}
	out := make([]dispatch.Candidate, 0, len(replies))
	for _, n := range ExampleNames(replies) {
		c, ok := byName[n]
		if !ok {
			c = dispatch.Candidate{Name: n}
		}
		out = append(out, c)
	}
	return out
}

// defaultReply is "default", else the first reply without a request half,
// else the first reply.
func defaultReply(names []string, cands []dispatch.Candidate) string {
	if slices.Contains(names, "default") {
		return "default"
	}
	for _, c := range cands {
		if c.Body == nil && len(c.Headers) == 0 {
			return c.Name
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
