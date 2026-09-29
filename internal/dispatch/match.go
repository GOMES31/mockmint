package dispatch

import (
	"errors"
	"fmt"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Matcher tests one string value. In YAML it is either a scalar (exact
// match) or a mapping with exactly one of equals, regex, in, exists:
//
//	petId: "1"
//	petId: {regex: "^9\\d+$"}
//	petId: {in: ["1", "2"]}
//	petId: {exists: false}
type Matcher struct {
	Equals *string  `yaml:"equals"`
	Regex  string   `yaml:"regex"`
	In     []string `yaml:"in"`
	Exists *bool    `yaml:"exists"`

	re *regexp.Regexp
}

// UnmarshalYAML accepts the scalar shorthand or the mapping form.
func (m *Matcher) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v := n.Value
		*m = Matcher{Equals: &v}
		return nil
	}
	type plain Matcher
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*m = Matcher(p)
	return nil
}

// Eq returns an exact-match Matcher.
func Eq(v string) Matcher { return Matcher{Equals: &v} }

func (m *Matcher) compile() error {
	set := 0
	if m.Equals != nil {
		set++
	}
	if m.Regex != "" {
		set++
		re, err := regexp.Compile(m.Regex)
		if err != nil {
			return fmt.Errorf("regex: %w", err)
		}
		m.re = re
	}
	if m.In != nil {
		set++
	}
	if m.Exists != nil {
		set++
	}
	if set != 1 {
		return errors.New("matcher needs exactly one of equals, regex, in, exists")
	}
	return nil
}

// match tests value; present is false when the value is absent.
func (m *Matcher) match(value string, present bool) bool {
	switch {
	case m.Exists != nil:
		return present == *m.Exists
	case !present:
		return false
	case m.Equals != nil:
		return value == *m.Equals
	case m.re != nil:
		return m.re.MatchString(value)
	default:
		return slices.Contains(m.In, value)
	}
}

// matchAny tests a multi-valued input (query, header): it matches when any
// value matches, or on presence for exists.
func (m *Matcher) matchAny(values []string) bool {
	if m.Exists != nil || len(values) == 0 {
		return m.match("", len(values) > 0)
	}
	for _, v := range values {
		if m.match(v, true) {
			return true
		}
	}
	return false
}
