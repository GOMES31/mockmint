package asyncapi

import "go.yaml.in/yaml/v3"

// Raw document structs. Fields that may be a $ref are kept as yaml.Node and
// resolved during normalization. They are values, not *yaml.Node: yaml.v3
// leaves *yaml.Node fields empty when decoding from a node rather than bytes.
// An absent field has Kind 0.

type info struct {
	Title   string `yaml:"title"`
	Version string `yaml:"version"`
}

type header struct {
	AsyncAPI string `yaml:"asyncapi"`
	Info     info   `yaml:"info"`
}

// --- AsyncAPI 2.6 ------------------------------------------------------------

type docV2 struct {
	Channels map[string]yaml.Node `yaml:"channels"`
}

type channelV2 struct {
	Bindings  yaml.Node    `yaml:"bindings"`
	Publish   *operationV2 `yaml:"publish"`
	Subscribe *operationV2 `yaml:"subscribe"`
}

type operationV2 struct {
	OperationID string    `yaml:"operationId"`
	Bindings    yaml.Node `yaml:"bindings"`
	Message     yaml.Node `yaml:"message"` // a message, or {oneOf: [...]}
}

// --- AsyncAPI 3.0 ------------------------------------------------------------

type docV3 struct {
	Channels   map[string]yaml.Node `yaml:"channels"`
	Operations map[string]yaml.Node `yaml:"operations"`
}

type channelV3 struct {
	Address  *string              `yaml:"address"`
	Messages map[string]yaml.Node `yaml:"messages"`
	Bindings yaml.Node            `yaml:"bindings"`
}

type operationV3 struct {
	Action   string      `yaml:"action"`
	Channel  yaml.Node   `yaml:"channel"`
	Messages []yaml.Node `yaml:"messages"`
	Reply    yaml.Node   `yaml:"reply"`
	Bindings yaml.Node   `yaml:"bindings"`
}

type replyV3 struct {
	Channel  yaml.Node   `yaml:"channel"`
	Messages []yaml.Node `yaml:"messages"`
}

// --- shared ------------------------------------------------------------------

// messageDoc covers the message fields mockmint uses in both versions.
type messageDoc struct {
	Name         string       `yaml:"name"`
	MessageID    string       `yaml:"messageId"` // 2.4+
	ContentType  string       `yaml:"contentType"`
	SchemaFormat string       `yaml:"schemaFormat"` // 2.x; 3.0 uses a multi-format payload
	Examples     []exampleDoc `yaml:"examples"`
	Bindings     yaml.Node    `yaml:"bindings"`
}

type exampleDoc struct {
	Name    string         `yaml:"name"`
	Payload yaml.Node      `yaml:"payload"`
	Headers map[string]any `yaml:"headers"`
}

type bindingsDoc struct {
	AMQP yaml.Node `yaml:"amqp"`
}

// ChannelBinding is the AMQP channel binding (bindingVersion 0.2.0/0.3.0).
type ChannelBinding struct {
	// Is is "routingKey" (publish to an exchange with the channel address as
	// routing key, the default) or "queue" (the channel is a queue).
	Is       string   `yaml:"is"`
	Exchange Exchange `yaml:"exchange"`
	Queue    Queue    `yaml:"queue"`
}

// Exchange is an exchange declaration. Durable defaults to true.
type Exchange struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"` // topic, direct, fanout, default, headers
	Durable    *bool  `yaml:"durable"`
	AutoDelete bool   `yaml:"autoDelete"`
	VHost      string `yaml:"vhost"`
}

// Queue is a queue declaration. Durable defaults to true.
type Queue struct {
	Name       string `yaml:"name"`
	Durable    *bool  `yaml:"durable"`
	Exclusive  bool   `yaml:"exclusive"`
	AutoDelete bool   `yaml:"autoDelete"`
	VHost      string `yaml:"vhost"`
}

// OperationBinding is the AMQP operation binding; mockmint applies these
// when it publishes on the operation.
type OperationBinding struct {
	Expiration   int      `yaml:"expiration"` // ms
	UserID       string   `yaml:"userId"`
	CC           []string `yaml:"cc"`
	Priority     uint8    `yaml:"priority"`
	DeliveryMode uint8    `yaml:"deliveryMode"` // 1 transient, 2 persistent
	Mandatory    bool     `yaml:"mandatory"`
	Timestamp    bool     `yaml:"timestamp"`
}

// MessageBinding is the AMQP message binding.
type MessageBinding struct {
	ContentEncoding string `yaml:"contentEncoding"`
	MessageType     string `yaml:"messageType"`
}
