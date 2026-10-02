// Package schema compiles JSON Schema documents for a namespace's config schema.
//
// Compilation is deliberately hermetic: the jsonschema library's default loader is
// FileLoader, so a document containing `"$ref":"file:///etc/passwd"` would read local
// files at compile time. Compile installs a loader that refuses every URL, leaving
// only the standard meta-schemas — which the library embeds — resolvable.
package schema

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compiledSchemaURL is the synthetic, absolute URL the document is registered under
// before compilation. It is never fetched: AddResource seeds it in the compiler's
// in-memory document store.
const compiledSchemaURL = "urn:config:schema"

// denyLoader is the compiler's URLLoader. Every external reference is an error, so
// only the library's built-in meta-schemas resolve.
type denyLoader struct{}

// Load always fails. The returned error never carries the referenced resource's
// contents because none are read.
func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("reference to %q is not allowed", url)
}

// Compile turns raw into a compiled validator.
//
// It rejects anything that is not a valid JSON object before handing it to the
// compiler, and rejects anything that is not a valid JSON Schema during compilation.
func Compile(raw []byte) (*jsonschema.Schema, error) {
	// IsValid with default options rejects duplicate object member names and invalid
	// UTF-8; both are easy to smuggle past a plain Unmarshal and neither is legal JSON.
	if !jsontext.Value(raw).IsValid() {
		return nil, errors.New("body is not valid JSON")
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode document: %w", err)
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, errors.New("top-level value must be a JSON object")
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(denyLoader{})
	if err := compiler.AddResource(compiledSchemaURL, doc); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}

	compiled, err := compiler.Compile(compiledSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return compiled, nil
}

// Cache holds compiled validators keyed by (namespace, schemaVersion).
//
// A schema version is immutable once written, so an entry never needs invalidating
// or replacing. The zero value is ready to use.
type Cache struct {
	mu    sync.RWMutex
	items map[cacheKey]*jsonschema.Schema
}

// cacheKey identifies one immutable schema version.
type cacheKey struct {
	namespace string
	version   int
}

// Get returns the compiled validator for (ns, version), compiling raw on a miss and
// caching the result. Callers must pass the raw body that version stored.
//
// A nil *Cache is valid and compiles without caching, so a server or test built without
// one still validates correctly; only the reuse is lost.
func (c *Cache) Get(ns string, version int, raw []byte) (*jsonschema.Schema, error) {
	if c == nil {
		return Compile(raw)
	}

	key := cacheKey{namespace: ns, version: version}

	c.mu.RLock()
	compiled, ok := c.items[key]
	c.mu.RUnlock()
	if ok {
		return compiled, nil
	}

	compiled, err := Compile(raw)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[cacheKey]*jsonschema.Schema)
	}
	// Another goroutine may have compiled the same version while this one worked;
	// return the first stored validator so the pointer identity is stable.
	if existing, ok := c.items[key]; ok {
		return existing, nil
	}
	c.items[key] = compiled
	return compiled, nil
}
