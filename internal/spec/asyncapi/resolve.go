package asyncapi

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxRefHops bounds $ref chains (a → b → c …) and catches ref loops.
const maxRefHops = 32

// loc is where a node lives: a package file and a JSON pointer in it.
type loc struct {
	file string
	ptr  string // "" is the document root; otherwise "/a/b"
}

func (l loc) child(key string) loc {
	return loc{file: l.file, ptr: l.ptr + "/" + escapePtr(key)}
}

func (l loc) index(i int) loc { return l.child(strconv.Itoa(i)) }

func (l loc) String() string { return l.file + "#" + l.ptr }

// docSet loads package files on demand and resolves $refs between them.
// Every file must stay inside fsys.
type docSet struct {
	fsys  fs.FS
	files map[string]*yaml.Node // file → document content node
}

func newDocSet(fsys fs.FS) *docSet {
	return &docSet{fsys: fsys, files: map[string]*yaml.Node{}}
}

func (d *docSet) file(name string) (*yaml.Node, error) {
	if n, ok := d.files[name]; ok {
		return n, nil
	}
	b, err := fs.ReadFile(d.fsys, name)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: empty document", name)
	}
	d.files[name] = doc.Content[0]
	return doc.Content[0], nil
}

// resolve follows n's $ref chain (if any) and returns the target and its
// location. Non-ref nodes are returned unchanged.
func (d *docSet) resolve(n *yaml.Node, at loc) (*yaml.Node, loc, error) {
	for range maxRefHops {
		n = deAlias(n)
		ref, ok := refOf(n)
		if !ok {
			return n, at, nil
		}
		target, err := d.target(ref, at)
		if err != nil {
			return nil, at, fmt.Errorf("%s: $ref %q: %w", at, ref, err)
		}
		root, err := d.file(target.file)
		if err != nil {
			return nil, at, fmt.Errorf("%s: $ref %q: %w", at, ref, err)
		}
		if n, err = lookup(root, target.ptr); err != nil {
			return nil, at, fmt.Errorf("%s: $ref %q: %w", at, ref, err)
		}
		at = target
	}
	return nil, at, fmt.Errorf("%s: $ref chain longer than %d (loop?)", at, maxRefHops)
}

// target turns a $ref string into a location relative to at.
func (d *docSet) target(ref string, at loc) (loc, error) {
	file, frag, _ := strings.Cut(ref, "#")
	if strings.Contains(file, "://") || strings.HasPrefix(file, "//") {
		return loc{}, errors.New("remote references are not allowed")
	}
	if file == "" {
		file = at.file
	} else {
		if path.IsAbs(file) {
			return loc{}, errors.New("absolute references are not allowed")
		}
		file = path.Clean(path.Join(path.Dir(at.file), file))
		if !fs.ValidPath(file) {
			return loc{}, errors.New("reference escapes the package")
		}
	}
	if frag != "" && !strings.HasPrefix(frag, "/") {
		return loc{}, errors.New("only JSON pointer fragments are supported")
	}
	return loc{file: file, ptr: frag}, nil
}

func refOf(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "$ref" {
			return n.Content[i+1].Value, true
		}
	}
	return "", false
}

func deAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// lookup evaluates a JSON pointer against root.
func lookup(root *yaml.Node, ptr string) (*yaml.Node, error) {
	n := deAlias(root)
	if ptr == "" {
		return n, nil
	}
	for tok := range strings.SplitSeq(ptr[1:], "/") {
		key := unescapePtr(tok)
		switch n.Kind {
		case yaml.MappingNode:
			next := get(n, key)
			if next == nil {
				return nil, fmt.Errorf("pointer %q: no key %q", ptr, key)
			}
			n = next
		case yaml.SequenceNode:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(n.Content) {
				return nil, fmt.Errorf("pointer %q: bad index %q", ptr, key)
			}
			n = deAlias(n.Content[i])
		default:
			return nil, fmt.Errorf("pointer %q: cannot descend into a scalar at %q", ptr, key)
		}
	}
	return n, nil
}

// get returns the value for key in a mapping node, or nil.
func get(n *yaml.Node, key string) *yaml.Node {
	n = deAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return deAlias(n.Content[i+1])
		}
	}
	return nil
}

// keys returns a mapping node's keys in document order.
func keys(n *yaml.Node) []string {
	n = deAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, n.Content[i].Value)
	}
	return out
}

func escapePtr(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescapePtr(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}
