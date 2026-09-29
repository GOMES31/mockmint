// Package config loads the mockmint server configuration from a YAML file and
// applies MOCKMINT_* environment variable overrides.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
