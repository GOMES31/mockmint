// Package config loads the mockmint server configuration from a YAML file and
// applies MOCKMINT_* environment variable overrides.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix prefixes every environment override. A field's variable name is
// the prefix plus its YAML path joined by underscores and upper-cased, e.g.
// http.addr → MOCKMINT_HTTP_ADDR, packages.paths → MOCKMINT_PACKAGES_PATHS.
const EnvPrefix = "MOCKMINT"

// Config is the server configuration.
type Config struct {
	HTTP     HTTP     `yaml:"http"`
	Admin    Admin    `yaml:"admin"`
	AMQP     AMQP     `yaml:"amqp"`
	Log      Log      `yaml:"log"`
	Packages Packages `yaml:"packages"`
	Defaults Defaults `yaml:"defaults"`
}

// HTTP configures the mock traffic listener.
type HTTP struct {
	Addr              string   `yaml:"addr"`
	ReadHeaderTimeout Duration `yaml:"readHeaderTimeout"`
	IdleTimeout       Duration `yaml:"idleTimeout"`
	ShutdownTimeout   Duration `yaml:"shutdownTimeout"`
	MaxBodyBytes      int64    `yaml:"maxBodyBytes"`
}

// Admin configures the admin API listener (health, metrics, management).
type Admin struct {
	Addr string `yaml:"addr"` // "" disables the admin listener
	// Token, when set, is required as "Authorization: Bearer <token>" on
	// /admin/*. Health, readiness and metrics stay open for probes.
	Token string `yaml:"token"`
	// DataDir persists uploaded packages across restarts ("" = memory only).
	DataDir        string   `yaml:"dataDir"`
	MaxUploadBytes int64    `yaml:"maxUploadBytes"`
	TrafficSize    int      `yaml:"trafficSize"`   // recent exchanges kept
	RedactHeaders  []string `yaml:"redactHeaders"` // in addition to the defaults
	RecordLimit    int      `yaml:"recordLimit"`   // proxied recordings kept per package
}

// Exposed reports whether the admin listener is reachable from other hosts:
// any address that is not a loopback IP or "localhost". Unparsable
// addresses count as exposed.
func (a Admin) Exposed() bool {
	if a.Addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(a.Addr)
	if err != nil {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// AMQP configures the RabbitMQ engine. With an empty URL the engine is off
// and mockmint serves HTTP only.
type AMQP struct {
	URL            string   `yaml:"url"` // amqp://user:pass@host:5672/vhost
	Prefetch       int      `yaml:"prefetch"`
	Heartbeat      Duration `yaml:"heartbeat"`
	ReconnectMin   Duration `yaml:"reconnectMin"`
	ReconnectMax   Duration `yaml:"reconnectMax"`
	ConfirmTimeout Duration `yaml:"confirmTimeout"`
}

// Log configures log/slog output.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // json, text
}

// Packages lists mock packages to load at startup. Each path is a package
// directory, a .zip/.tar.gz archive, or a directory whose subdirectories and
// archives are each a package.
type Packages struct {
	Paths []string `yaml:"paths"`
}

// Defaults apply to packages that do not set the value in mockmint.yaml.
type Defaults struct {
	Validation string `yaml:"validation"` // strict, warn, off
	Seed       int64  `yaml:"seed"`       // 0 = non-deterministic
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		HTTP: HTTP{
			Addr:              ":8080",
			ReadHeaderTimeout: Duration(5 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			ShutdownTimeout:   Duration(10 * time.Second),
			MaxBodyBytes:      10 << 20,
		},
		Admin: Admin{
			Addr:           "127.0.0.1:9090",
			MaxUploadBytes: 64 << 20,
			TrafficSize:    200,
			RecordLimit:    100,
		},
		AMQP: AMQP{
			Prefetch:       10,
			Heartbeat:      Duration(10 * time.Second),
			ReconnectMin:   Duration(500 * time.Millisecond),
			ReconnectMax:   Duration(30 * time.Second),
			ConfirmTimeout: Duration(5 * time.Second),
		},
		Log:      Log{Level: "info", Format: "json"},
		Defaults: Defaults{Validation: "warn"},
	}
}

// Load builds the configuration: defaults, then the YAML file at path (if
// non-empty), then environment overrides from environ (KEY=VALUE pairs, as
// returned by os.Environ).
func Load(path string, environ []string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the operator chooses the config file
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, environ); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// Validate reports invalid values.
func (c Config) Validate() error {
	var errs []error
	if c.HTTP.Addr == "" {
		errs = append(errs, errors.New("http.addr must not be empty"))
	}
	if c.HTTP.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("http.maxBodyBytes must be positive"))
	}
	if c.AMQP.URL != "" {
		if u, err := url.Parse(c.AMQP.URL); err != nil || (u.Scheme != "amqp" && u.Scheme != "amqps") {
			errs = append(errs, errors.New("amqp.url must be an amqp:// or amqps:// URL"))
		}
	}
	// Port 0 picks a free port, so equal ":0" addresses do not collide.
	if c.Admin.Addr != "" && c.Admin.Addr == c.HTTP.Addr && !strings.HasSuffix(c.Admin.Addr, ":0") {
		errs = append(errs, errors.New("admin.addr must differ from http.addr"))
	}
	if c.Admin.MaxUploadBytes <= 0 || c.Admin.TrafficSize < 1 || c.Admin.RecordLimit < 1 {
		errs = append(errs, errors.New("admin.maxUploadBytes, trafficSize and recordLimit must be positive"))
	}
	if c.AMQP.Prefetch < 1 || c.AMQP.Prefetch > 65535 {
		errs = append(errs, errors.New("amqp.prefetch must be between 1 and 65535"))
	}
	if c.AMQP.ReconnectMin <= 0 || c.AMQP.ReconnectMax < c.AMQP.ReconnectMin {
		errs = append(errs, errors.New("amqp: need 0 < reconnectMin <= reconnectMax"))
	}
	if c.AMQP.ConfirmTimeout <= 0 {
		errs = append(errs, errors.New("amqp.confirmTimeout must be positive"))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format %q: want json or text", c.Log.Format))
	}
	switch c.Defaults.Validation {
	case "strict", "warn", "off":
	default:
		errs = append(errs, fmt.Errorf("defaults.validation %q: want strict, warn or off", c.Defaults.Validation))
	}
	return errors.Join(errs...)
}

// Duration is a time.Duration that reads and writes Go duration strings ("5s").
type Duration time.Duration

// D returns d as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML accepts a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes d as a duration string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

var durationType = reflect.TypeFor[Duration]()

func applyEnv(cfg *Config, environ []string) error {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, EnvPrefix+"_") {
			env[k] = v
		}
	}
	if len(env) == 0 {
		return nil
	}
	var errs []error
	walk(reflect.ValueOf(cfg).Elem(), EnvPrefix, func(name string, f reflect.Value) {
		raw, ok := env[name]
		if !ok {
			return
		}
		delete(env, name)
		if err := setField(f, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	})
	for k := range env {
		errs = append(errs, fmt.Errorf("%s: unknown configuration variable", k))
	}
	return errors.Join(errs...)
}

func walk(v reflect.Value, prefix string, fn func(string, reflect.Value)) {
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + "_" + strings.ToUpper(tag)
		f := v.Field(i)
		if f.Kind() == reflect.Struct && f.Type() != durationType {
			walk(f, name, fn)
			continue
		}
		fn(name, f)
	}
}

func setField(f reflect.Value, raw string) error {
	if f.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		f.SetInt(int64(d))
		return nil
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString(raw)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		f.SetInt(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case reflect.Slice:
		var items []string
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		f.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("unsupported field kind %s", f.Kind())
	}
	return nil
}
