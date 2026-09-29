package template

// StateReader gives templates read access to the package's state.
type StateReader interface {
	Get(collection, key string) (any, bool)
	List(collection string) []any
}

// State is exposed to templates as .State. Value and Found hold the entry
// a stateful read/update operation resolved; Get and List read any
// collection of the package.
type State struct {
	Value any
	Found bool

	reader StateReader
}

// NewState returns a template state view.
func NewState(value any, found bool, r StateReader) State {
	return State{Value: value, Found: found, reader: r}
}

// Get returns a stored value, or nil. Usage: {{ .State.Get "notes" "1" }}.
func (s State) Get(collection, key string) any {
	if s.reader == nil {
		return nil
	}
	v, _ := s.reader.Get(collection, key)
	return v
}

// List returns a collection's values in key order.
func (s State) List(collection string) []any {
	if s.reader == nil {
		return nil
	}
	return s.reader.List(collection)
}
