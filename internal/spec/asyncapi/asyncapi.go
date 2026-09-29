// Package asyncapi loads AsyncAPI 2.6 and 3.0 documents into one
// version-independent model of channels, operations and messages, with the
// AMQP bindings mockmint needs to mock RabbitMQ.
package asyncapi

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Actions, from the point of view of the application the document
// describes, which is the role mockmint plays.
const (
	ActionSend    = "send"
	ActionReceive = "receive"
)

// Spec is a loaded AsyncAPI document.
type Spec struct {
	AsyncAPI   string // document version, e.g. "3.0.0"
	Title      string
	Version    string // info.version
	Channels   []*Channel
	Operations []*Operation // sorted by ID
	Warnings   []string
}

// Channel is an addressable channel with its AMQP binding.
type Channel struct {
	ID      string // channel key
	Address string // routing key or queue name; "" when unknown (3.0 null address)
	AMQP    ChannelBinding
}

// Operation is something the application sends or receives.
type Operation struct {
	ID       string
	Action   string
	Channel  *Channel
	Messages []*Message
	Reply    *Reply // AsyncAPI 3.0 reply, nil otherwise
	AMQP     OperationBinding
}

// Reply describes the reply to a received message (AsyncAPI 3.0).
type Reply struct {
	Channel  *Channel // nil: reply to the request's reply_to
	Messages []*Message
}

// Message is a message definition.
type Message struct {
	ID          string
	ContentType string
	Payload     *Schema // nil when absent or in an unsupported schema format
	Headers     *Schema
	Examples    []Example
	AMQP        MessageBinding
}

// Example is a named message example.
type Example struct {
	Name       string
	Payload    any // JSON-compatible; nil when HasPayload is false
	HasPayload bool
	Headers    map[string]any
}

// IsDocument reports whether data looks like an AsyncAPI document.
func IsDocument(data []byte) bool {
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("asyncapi:")) || bytes.HasPrefix(t, []byte(`"asyncapi"`)) || bytes.HasPrefix(t, []byte(`{"asyncapi"`)) {
			return true
		}
	}
	return false
}

// Load reads the document at name from fsys.
func Load(fsys fs.FS, name string) (*Spec, error) {
	l := &loader{
		docs:     newDocSet(fsys),
		schemas:  newSchemaSet(fsys),
		channels: map[string]*Channel{},
		messages: map[string]*Message{},
	}
	s, err := l.load(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return s, nil
}

type loader struct {
	docs     *docSet
	schemas  *schemaSet
	spec     *Spec
	channels map[string]*Channel // by location, so $refs share one Channel
	messages map[string]*Message // by location
}

func (l *loader) load(name string) (*Spec, error) {
	root, err := l.docs.file(name)
	if err != nil {
		return nil, err
	}
	var h header
	if err := root.Decode(&h); err != nil {
		return nil, err
	}
	l.spec = &Spec{AsyncAPI: h.AsyncAPI, Title: h.Info.Title, Version: h.Info.Version}
	at := loc{file: name}
	switch {
	case strings.HasPrefix(h.AsyncAPI, "2."):
		if !strings.HasPrefix(h.AsyncAPI, "2.6") {
			l.warnf("AsyncAPI %s is read as 2.6", h.AsyncAPI)
		}
		err = l.loadV2(root, at)
	case strings.HasPrefix(h.AsyncAPI, "3."):
		err = l.loadV3(root, at)
	case h.AsyncAPI == "":
		return nil, errors.New("not an AsyncAPI document (no asyncapi field)")
	default:
		return nil, fmt.Errorf("unsupported AsyncAPI version %q (want 2.6 or 3.0)", h.AsyncAPI)
	}
	if err != nil {
		return nil, err
	}
	if len(l.spec.Operations) == 0 {
		return nil, errors.New("no operations")
	}
	slices.SortFunc(l.spec.Operations, func(a, b *Operation) int { return cmp.Compare(a.ID, b.ID) })
	for _, c := range l.channels {
		l.spec.Channels = append(l.spec.Channels, c)
	}
	slices.SortFunc(l.spec.Channels, func(a, b *Channel) int { return cmp.Compare(a.ID, b.ID) })
	seen := map[string]bool{}
	for _, op := range l.spec.Operations {
		if seen[op.ID] {
			return nil, fmt.Errorf("duplicate operation id %q", op.ID)
		}
		seen[op.ID] = true
	}
	return l.spec, nil
}

func (l *loader) warnf(format string, args ...any) {
	l.spec.Warnings = append(l.spec.Warnings, fmt.Sprintf(format, args...))
}

// decode resolves n and decodes it into v, returning the resolved node's
// location.
func (l *loader) decode(n *yaml.Node, at loc, v any) (*yaml.Node, loc, error) {
	rn, rat, err := l.docs.resolve(n, at)
	if err != nil {
		return nil, at, err
	}
	if err := rn.Decode(v); err != nil {
		return nil, at, fmt.Errorf("%s: %w", rat, err)
	}
	return rn, rat, nil
}

// --- 2.6 ---------------------------------------------------------------------

func (l *loader) loadV2(root *yaml.Node, at loc) error {
	var d docV2
	if err := root.Decode(&d); err != nil {
		return err
	}
	for _, key := range sortedKeys(d.Channels) {
		n := d.Channels[key]
		cat := at.child("channels").child(key)
		var ch channelV2
		rn, rat, err := l.decode(&n, cat, &ch)
		if err != nil {
			return err
		}
		c, err := l.channel(key, key, &ch.Bindings, rat)
		if err != nil {
			return err
		}
		// 2.x: "publish" = others publish to the app → the app receives;
		// "subscribe" = others subscribe → the app sends.
		for _, side := range []struct {
			op     *operationV2
			field  string
			action string
		}{{ch.Publish, "publish", ActionReceive}, {ch.Subscribe, "subscribe", ActionSend}} {
			if side.op == nil {
				continue
			}
			oat := rat.child(side.field)
			op := &Operation{ID: side.op.OperationID, Action: side.action, Channel: c}
			if op.ID == "" {
				op.ID = key + "." + side.field
			}
			if op.AMQP, err = l.operationBinding(&side.op.Bindings, oat.child("bindings")); err != nil {
				return err
			}
			msgs, err := l.messagesV2(get(rn, side.field), oat)
			if err != nil {
				return err
			}
			op.Messages = msgs
			l.spec.Operations = append(l.spec.Operations, op)
		}
	}
	return nil
}

// messagesV2 reads operation.message, which is one message or {oneOf: [...]}.
func (l *loader) messagesV2(opNode *yaml.Node, oat loc) ([]*Message, error) {
	mn := get(opNode, "message")
	if mn == nil {
		return nil, nil
	}
	mat := oat.child("message")
	rn, rat, err := l.docs.resolve(mn, mat)
	if err != nil {
		return nil, err
	}
	if one := get(rn, "oneOf"); one != nil && one.Kind == yaml.SequenceNode {
		var out []*Message
		for i, item := range one.Content {
			m, err := l.message(item, rat.child("oneOf").index(i), "")
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, nil
	}
	m, err := l.message(mn, mat, "")
	if err != nil {
		return nil, err
	}
	return []*Message{m}, nil
}

// --- 3.0 ---------------------------------------------------------------------

func (l *loader) loadV3(root *yaml.Node, at loc) error {
	var d docV3
	if err := root.Decode(&d); err != nil {
		return err
	}
	for _, key := range sortedKeys(d.Operations) {
		n := d.Operations[key]
		oat := at.child("operations").child(key)
		var od operationV3
		_, rat, err := l.decode(&n, oat, &od)
		if err != nil {
			return err
		}
		op := &Operation{ID: key, Action: od.Action}
		if op.Action != ActionSend && op.Action != ActionReceive {
			return fmt.Errorf("operation %s: action %q: want send or receive", key, od.Action)
		}
		if op.Channel, err = l.channelV3(&od.Channel, rat.child("channel")); err != nil {
			return fmt.Errorf("operation %s: %w", key, err)
		}
		if op.AMQP, err = l.operationBinding(&od.Bindings, rat.child("bindings")); err != nil {
			return err
		}
		if op.Messages, err = l.messageRefs(od.Messages, rat.child("messages")); err != nil {
			return fmt.Errorf("operation %s: %w", key, err)
		}
		if len(od.Messages) == 0 {
			// 3.0: omitted messages mean every message of the channel.
			if op.Messages, err = l.channelMessages(&od.Channel, rat.child("channel")); err != nil {
				return fmt.Errorf("operation %s: %w", key, err)
			}
		}
		if od.Reply.Kind != 0 {
			if op.Reply, err = l.reply(&od.Reply, rat.child("reply")); err != nil {
				return fmt.Errorf("operation %s reply: %w", key, err)
			}
		}
		l.spec.Operations = append(l.spec.Operations, op)
	}
	return nil
}

func (l *loader) reply(n *yaml.Node, at loc) (*Reply, error) {
	var rd replyV3
	_, rat, err := l.decode(n, at, &rd)
	if err != nil {
		return nil, err
	}
	r := &Reply{}
	if rd.Channel.Kind != 0 {
		if r.Channel, err = l.channelV3(&rd.Channel, rat.child("channel")); err != nil {
			return nil, err
		}
	}
	if r.Messages, err = l.messageRefs(rd.Messages, rat.child("messages")); err != nil {
		return nil, err
	}
	if len(rd.Messages) == 0 && r.Channel != nil {
		if r.Messages, err = l.channelMessages(&rd.Channel, rat.child("channel")); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (l *loader) channelV3(n *yaml.Node, at loc) (*Channel, error) {
	var cd channelV3
	_, rat, err := l.decode(n, at, &cd)
	if err != nil {
		return nil, err
	}
	id := lastToken(rat.ptr)
	addr := "" // 3.0: absent or null means unknown (e.g. the reply_to of a request)
	if cd.Address != nil {
		addr = *cd.Address
	}
	if strings.Contains(addr, "{") {
		l.warnf("channel %s: address %q has parameters; mockmint uses it literally", id, addr)
	}
	return l.channel(id, addr, &cd.Bindings, rat)
}

func (l *loader) channelMessages(chNode *yaml.Node, at loc) ([]*Message, error) {
	rn, rat, err := l.docs.resolve(chNode, at)
	if err != nil {
		return nil, err
	}
	msgs := get(rn, "messages")
	var out []*Message
	for _, k := range keys(msgs) {
		m, err := l.message(get(msgs, k), rat.child("messages").child(k), k)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (l *loader) messageRefs(refs []yaml.Node, at loc) ([]*Message, error) {
	out := make([]*Message, 0, len(refs))
	for i := range refs {
		m, err := l.message(&refs[i], at.index(i), "")
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// --- shared ------------------------------------------------------------------

func (l *loader) channel(id, addr string, bindings *yaml.Node, at loc) (*Channel, error) {
	if c, ok := l.channels[at.String()]; ok {
		return c, nil
	}
	c := &Channel{ID: id, Address: addr}
	if bindings.Kind != 0 {
		var b bindingsDoc
		_, bat, err := l.decode(bindings, at.child("bindings"), &b)
		if err != nil {
			return nil, err
		}
		if b.AMQP.Kind != 0 {
			if _, _, err := l.decode(&b.AMQP, bat.child("amqp"), &c.AMQP); err != nil {
				return nil, err
			}
		}
	}
	switch c.AMQP.Is {
	case "":
		c.AMQP.Is = "routingKey"
	case "routingKey", "queue":
	default:
		return nil, fmt.Errorf("channel %s: amqp binding is %q: want routingKey or queue", id, c.AMQP.Is)
	}
	l.channels[at.String()] = c
	return c, nil
}

func (l *loader) operationBinding(bindings *yaml.Node, at loc) (OperationBinding, error) {
	var ob OperationBinding
	if bindings.Kind == 0 {
		return ob, nil
	}
	var b bindingsDoc
	_, bat, err := l.decode(bindings, at, &b)
	if err != nil || b.AMQP.Kind == 0 {
		return ob, err
	}
	_, _, err = l.decode(&b.AMQP, bat.child("amqp"), &ob)
	return ob, err
}

// message resolves and loads a message. key names inline messages
// (a channel's messages map key) when the message has no name.
func (l *loader) message(n *yaml.Node, at loc, key string) (*Message, error) {
	rn, rat, err := l.docs.resolve(n, at)
	if err != nil {
		return nil, err
	}
	if m, ok := l.messages[rat.String()]; ok {
		return m, nil
	}
	rn, err = l.applyTraits(rn, rat)
	if err != nil {
		return nil, err
	}
	var md messageDoc
	if err := rn.Decode(&md); err != nil {
		return nil, fmt.Errorf("%s: %w", rat, err)
	}
	m := &Message{ContentType: md.ContentType}
	m.ID = cmp.Or(md.MessageID, md.Name, key, lastToken(rat.ptr))
	if md.Bindings.Kind != 0 {
		var b bindingsDoc
		_, bat, err := l.decode(&md.Bindings, rat.child("bindings"), &b)
		if err != nil {
			return nil, err
		}
		if b.AMQP.Kind != 0 {
			if _, _, err := l.decode(&b.AMQP, bat.child("amqp"), &m.AMQP); err != nil {
				return nil, err
			}
		}
	}
	if m.Payload, err = l.schema(get(rn, "payload"), rat.child("payload"), md.SchemaFormat, m.ID); err != nil {
		return nil, err
	}
	if m.Headers, err = l.schema(get(rn, "headers"), rat.child("headers"), "", m.ID); err != nil {
		return nil, err
	}
	for i, ex := range md.Examples {
		e := Example{Name: ex.Name, Headers: normalizeMap(ex.Headers)}
		if e.Name == "" {
			e.Name = fmt.Sprintf("example%d", i+1)
		}
		if ex.Payload.Kind != 0 {
			var v any
			if err := ex.Payload.Decode(&v); err != nil {
				return nil, fmt.Errorf("%s: example %q: %w", rat, e.Name, err)
			}
			e.Payload, e.HasPayload = normalize(v), true
		}
		m.Examples = append(m.Examples, e)
		l.checkExample(m, e)
	}
	l.messages[rat.String()] = m
	return m, nil
}

// applyTraits merges message traits into a copy of the message node; the
// message's own fields win (AsyncAPI trait semantics, shallow).
func (l *loader) applyTraits(msg *yaml.Node, at loc) (*yaml.Node, error) {
	traits := get(msg, "traits")
	if traits == nil || traits.Kind != yaml.SequenceNode {
		return msg, nil
	}
	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: slices.Clone(msg.Content)}
	for i, t := range traits.Content {
		tn, _, err := l.docs.resolve(t, at.child("traits").index(i))
		if err != nil {
			return nil, err
		}
		for j := 0; j+1 < len(tn.Content); j += 2 {
			if get(merged, tn.Content[j].Value) == nil {
				merged.Content = append(merged.Content, tn.Content[j], tn.Content[j+1])
			}
		}
	}
	return merged, nil
}

func (l *loader) schema(n *yaml.Node, at loc, format, msgID string) (*Schema, error) {
	if n == nil {
		return nil, nil
	}
	rn, rat, err := l.docs.resolve(n, at)
	if err != nil {
		return nil, err
	}
	// 3.0 multi-format schema: {schemaFormat, schema}.
	if sf := get(rn, "schemaFormat"); sf != nil && get(rn, "schema") != nil {
		format = sf.Value
		rn, rat, err = l.docs.resolve(get(rn, "schema"), rat.child("schema"))
		if err != nil {
			return nil, err
		}
	}
	if !jsonSchemaFormat(format) {
		l.warnf("message %s: schema format %q is not supported; payloads are not validated", msgID, format)
		return nil, nil
	}
	return l.schemas.compile(l.docs, rn, rat)
}

// checkExample warns when an example does not match its schemas. Templated
// examples are checked after rendering instead.
func (l *loader) checkExample(m *Message, e Example) {
	if raw, err := json.Marshal(e); err == nil && bytes.Contains(raw, []byte("{{")) {
		return
	}
	if m.Payload != nil && e.HasPayload {
		if v := m.Payload.Validate(e.Payload); v != nil {
			l.warnf("message %s example %q does not match its payload schema: %s", m.ID, e.Name, v[0])
		}
	}
	if m.Headers != nil && e.Headers != nil {
		if v := m.Headers.Validate(e.Headers); v != nil {
			l.warnf("message %s example %q does not match its headers schema: %s", m.ID, e.Name, v[0])
		}
	}
}

func jsonSchemaFormat(f string) bool {
	f = strings.ToLower(f)
	return f == "" || strings.HasPrefix(f, "application/vnd.aai.asyncapi") ||
		strings.HasPrefix(f, "application/schema+json") || strings.HasPrefix(f, "application/schema+yaml")
}

func lastToken(ptr string) string {
	i := strings.LastIndexByte(ptr, '/')
	return unescapePtr(ptr[i+1:])
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
