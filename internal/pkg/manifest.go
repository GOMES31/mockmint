// Package pkg loads mock packages: an OpenAPI document, optional example
// files and an optional mockmint.yaml manifest, from a directory or archive.
package pkg

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mockmint/mockmint/internal/behavior"
	"github.com/mockmint/mockmint/internal/dispatch"
)

// ManifestFile is the package manifest's file name.
const ManifestFile = "mockmint.yaml"

// Validation modes.
const (
	ValidationStrict = "strict"
	ValidationWarn   = "warn"
	ValidationOff    = "off"
)

// Templating modes.
const (
	TemplatingAuto = "auto" // render bodies and headers that contain "{{"
	TemplatingOn   = "on"
	TemplatingOff  = "off"
)

// Manifest is mockmint.yaml. Every field is optional.
type Manifest struct {
	// Name and Version identify the package and form the default base path
	// /{name}/{version}. Defaults: the slugified spec title and info.version.
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// BasePath overrides where routes are mounted; "/" mounts them unprefixed.
	BasePath string `yaml:"basePath"`
	// Spec and AsyncAPI are the documents' paths in the package (default:
	// detected). A package needs at least one of them.
	Spec     string `yaml:"spec"`
	AsyncAPI string `yaml:"asyncapi"`
	// Validation is strict, warn or off (default: server default).
	Validation string `yaml:"validation"`
	// Seed makes random template values, latency and faults deterministic.
	Seed *int64 `yaml:"seed"`
	// Clock freezes template time (RFC 3339).
	Clock *time.Time `yaml:"clock"`
	// Templating is auto, on or off (default auto).
	Templating string `yaml:"templating"`

	Behavior behavior.Config `yaml:"behavior"`
	Fallback *Fallback       `yaml:"fallback"`
	Proxy    *ProxyConfig    `yaml:"proxy"`
	// InitialState seeds stateful collections: collection → key → value.
	// A collection is seeded only while it is empty.
	InitialState map[string]map[string]any     `yaml:"initialState"`
	Operations   map[string]OperationOverrides `yaml:"operations"`

	// Async configures the AsyncAPI (RabbitMQ) side of the package.
	Async *AsyncManifest `yaml:"async"`
}

// OperationOverrides configure one operation, keyed in the manifest by
// "METHOD /path" (as written in the spec) or operationId.
type OperationOverrides struct {
	Dispatcher dispatch.Config `yaml:"dispatcher"`
	Behavior   behavior.Config `yaml:"behavior"`
	Fallback   *Fallback       `yaml:"fallback"`
	Validation string          `yaml:"validation"`
	State      *StateConfig    `yaml:"state"`
}

// Fallback is the response when the dispatcher finds no example: either a
// named example, or an inline response. Without one, mockmint answers 404
// application/problem+json.
type Fallback struct {
	Example   string            `yaml:"example"`
	Status    int               `yaml:"status"`
	MediaType string            `yaml:"mediaType"`
	Headers   map[string]string `yaml:"headers"`
	Body      Body              `yaml:"body"`
}

// Body is a response or request body in YAML: a string is used verbatim,
// anything else is encoded as JSON.
type Body struct {
	Set   bool
	Raw   []byte
	Value any // decoded structured value; nil for string bodies
}

// UnmarshalYAML captures strings verbatim and structured values as JSON.
func (b *Body) UnmarshalYAML(n *yaml.Node) error {
	b.Set = true
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		b.Raw = []byte(n.Value)
		return nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return err
	}
	b.Value = v
	raw, err := marshalJSON(v)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	b.Raw = raw
	return nil
}

// ExampleFile is an examples/*.yaml|json file.
type ExampleFile struct {
	// Operation is "METHOD /path" or an operationId.
	Operation string                 `yaml:"operation"`
	Examples  map[string]FileExample `yaml:"examples"`
}

// FileExample is one named example in an example file.
type FileExample struct {
	Request struct {
		Params  map[string]string `yaml:"params"`
		Query   map[string]string `yaml:"query"`
		Headers map[string]string `yaml:"headers"`
		Body    Body              `yaml:"body"`
	} `yaml:"request"`
	Response struct {
		Status    int               `yaml:"status"`
		MediaType string            `yaml:"mediaType"`
		Headers   map[string]string `yaml:"headers"`
		Body      Body              `yaml:"body"`
	} `yaml:"response"`
}
