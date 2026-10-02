package schema

import (
	"bytes"
	"errors"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxValidateIssues bounds the number of leaf failures returned for one document. A
// client renders a form, not a novel: past the first screenful the list is noise, and
// the bound keeps a document that fails on every one of its thousands of fields from
// producing an unbounded response body.
const maxValidateIssues = 100

// Validate checks raw against a compiled schema.
//
// raw is decoded with jsonschema.UnmarshalJSON rather than encoding/json so numbers
// compare exactly; a float64 round trip could turn a valid 9007199254740993 into a
// value the schema rejects, or the reverse. The returned issues preserve the library's
// order and are capped at maxValidateIssues.
//
// A nil error means the document is valid. A *jsonschema.ValidationError becomes issues
// with a nil error; any other error is returned as-is for the caller to treat as a
// fault rather than as a statement about the document.
func Validate(compiled *jsonschema.Schema, raw []byte) ([]Issue, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	if err := compiled.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return nil, err
		}
		issues := Issues(err)
		if len(issues) > maxValidateIssues {
			issues = issues[:maxValidateIssues]
		}
		return issues, nil
	}
	return nil, nil
}
