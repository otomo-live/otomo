package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/config/internal/schema"
)

func TestIssuesLocateEachLeafFailure(t *testing.T) {
	tests := []struct {
		name, raw, wantPointer, wantMessagePart string
	}{
		{"top-level type is not a type name", `{"type":12}`, "/type", "want array"},
		{"nested typo in a type name", `{"properties":{"damage":{"type":"integr"}}}`, "/properties/damage/type", "must be one of"},
		{"minimum is not a number", `{"minimum":"x"}`, "/minimum", "want number"},
		{"pointer tokens are escaped", `{"properties":{"a/b~c":{"type":1}}}`, "/properties/a~1b~0c/type", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := schema.Compile([]byte(tc.raw))
			if err == nil {
				t.Fatal("Compile accepted an invalid schema")
			}
			issues := schema.Issues(err)
			if len(issues) == 0 {
				t.Fatalf("no issues extracted from %q", err)
			}
			for _, is := range issues {
				if is.Pointer == tc.wantPointer && strings.Contains(is.Message, tc.wantMessagePart) {
					return
				}
			}
			t.Errorf("no issue at %q containing %q; got %+v", tc.wantPointer, tc.wantMessagePart, issues)
		})
	}
}

func TestIssuesIsNilForNonValidationErrors(t *testing.T) {
	_, err := schema.Compile([]byte(`{"$ref":"file:///etc/passwd"}`))
	if err == nil {
		t.Fatal("expected a load error")
	}
	if got := schema.Issues(err); got != nil {
		t.Errorf("Issues(load error) = %+v, want nil", got)
	}
	if got := schema.Issues(errors.New("plain")); got != nil {
		t.Errorf("Issues(plain error) = %+v, want nil", got)
	}
}
