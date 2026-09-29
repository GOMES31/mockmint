package openapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

// Violation is one request validation failure.
type Violation struct {
	In      string `json:"in"`             // path, query, header, cookie, body, request
	Name    string `json:"name,omitempty"` // parameter name or body JSON pointer
	Message string `json:"message"`
}

// ValidateRequest checks r against the operation: parameters, and the
// request body when body is non-nil. pathParams are the raw path parameter
// values. It returns nil when the request is valid. Security requirements
// are not enforced by the mock.
func (o *Operation) ValidateRequest(ctx context.Context, r *http.Request, pathParams map[string]string) []Violation {
	in := &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: pathParams,
		Route: &routers.Route{
			Spec:      o.Doc,
			Path:      o.Path,
			PathItem:  o.PathItem,
			Method:    o.Method,
			Operation: o.Op,
		},
		Options: &openapi3filter.Options{
			MultiError:         true,
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}
	err := openapi3filter.ValidateRequest(ctx, in)
	if err == nil {
		return nil
	}
	var out []Violation
	collect(err, &out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].In != out[j].In {
			return out[i].In < out[j].In
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func collect(err error, out *[]Violation) {
	// Type-assert rather than errors.As: a RequestError's chain also holds
	// the schema MultiError, and matching that first would lose the
	// parameter/body location.
	if multi, ok := err.(openapi3.MultiError); ok { //nolint:errorlint // only the top level; see above
		for _, e := range multi {
			collect(e, out)
		}
		return
	}
	var reqErr *openapi3filter.RequestError
	if errors.As(err, &reqErr) {
		v := Violation{In: "request", Message: reqErr.Reason}
		switch {
		case reqErr.Parameter != nil:
			v.In, v.Name = reqErr.Parameter.In, reqErr.Parameter.Name
		case reqErr.RequestBody != nil:
			v.In = "body"
		}
		if reqErr.Err != nil {
			// Schema errors inside a body or parameter expand to one
			// violation each, keeping the location.
			var inner openapi3.MultiError
			if errors.As(reqErr.Err, &inner) {
				for _, e := range inner {
					*out = append(*out, withSchemaDetail(v, e))
				}
				return
			}
			v = withSchemaDetail(v, reqErr.Err)
		}
		if v.Message == "" {
			v.Message = oneLine(reqErr)
		}
		*out = append(*out, v)
		return
	}
	*out = append(*out, Violation{In: "request", Message: oneLine(err)})
}

func withSchemaDetail(v Violation, err error) Violation {
	var se *openapi3.SchemaError
	if errors.As(err, &se) {
		v.Message = se.Reason
		if ptr := se.JSONPointer(); len(ptr) > 0 && v.In == "body" {
			v.Name = "/" + strings.Join(ptr, "/")
		} else if ptr, msg, ok := splitJSONSchemaReason(se.Reason); ok {
			if v.In == "body" {
				v.Name = ptr
			}
			v.Message = msg
		}
		if v.Message == "" {
			v.Message = oneLine(se)
		}
		return v
	}
	if v.Message == "" {
		v.Message = oneLine(err)
	} else {
		v.Message += ": " + oneLine(err)
	}
	return v
}

// splitJSONSchemaReason parses the reason format kin-openapi produces for
// OpenAPI 3.1 documents (validated by santhosh-tekuri/jsonschema), which
// carries the pointer only in text: `error at "/name": at '/name': minLength: got 1, want 2`.
func splitJSONSchemaReason(reason string) (ptr, msg string, ok bool) {
	rest, found := strings.CutPrefix(reason, `error at "`)
	if !found {
		return "", "", false
	}
	ptr, rest, found = strings.Cut(rest, `": `)
	if !found {
		return "", "", false
	}
	msg = rest
	if after, cut := strings.CutPrefix(rest, "at '"+ptr+"': "); cut {
		msg = after
	}
	if ptr == "" {
		ptr = "/"
	}
	return ptr, oneLine(errors.New(msg)), true
}
