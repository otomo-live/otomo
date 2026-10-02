package schema

import (
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Issue is one leaf failure from the jsonschema library's error tree: where in the
// document it is, as an RFC 6901 JSON pointer, and what is wrong there. The pointer is
// what lets a client put each message on its own field.
type Issue struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

var printer = message.NewPrinter(language.English)

// Issues flattens a compile or validation error into its leaf failures, in the
// library's order. The library's own Error() nests every cause under a generic
// headline ("… is not valid against metaschema"), so showing a client only its first
// line would name nothing it can fix; the leaves are where the location lives.
//
// It returns nil for an error that did not come from schema validation (a load or
// JSON error), for which the caller should use err.Error().
func Issues(err error) []Issue {
	// A schema that fails its meta-schema arrives wrapped in SchemaValidationError,
	// which has no Unwrap, so errors.As cannot see through it.
	var sve *jsonschema.SchemaValidationError
	if errors.As(err, &sve) {
		err = sve.Err
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}

	var out []Issue
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, Issue{
				Pointer: pointer(e.InstanceLocation),
				Message: e.ErrorKind.LocalizedString(printer),
			})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// pointer renders an instance location as an RFC 6901 JSON pointer: "" for the
// document root, "~" escaped as "~0" and "/" as "~1" inside each token.
func pointer(tokens []string) string {
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteByte('/')
		sb.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(t))
	}
	return sb.String()
}
