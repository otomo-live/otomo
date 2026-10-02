package schema

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

// Canonical returns the canonical byte representation of raw: object members sorted
// by name, insignificant whitespace removed, and strings in their minimal form. It is
// what a version's bytes — and therefore its sha256 — are computed over.
//
// The value is formatted with jsontext.Value.Format and ReorderRawObjects rather than
// Canonicalize, deliberately. Canonicalize implements RFC 8785, which treats every
// number as an IEEE 754 double and would silently rewrite an integer above 2^53: a
// config document containing 9007199254740993 would hash and be stored as
// 9007199254740992. So integers keep their literal text (CanonicalizeRawInts stays
// off), while floats ARE canonicalized: Postgres jsonb may re-print a float it stores
// (1e2 comes back as 100), and Patch re-derives a manifest's bytes from jsonb before
// checking them against manifest_sha256. Both services must apply this exact rule —
// services/patch/internal/manifest has the same function — or a float in a manifest
// would make Patch refuse a perfectly good release.
//
// Duplicate object member names and invalid UTF-8 are rejected: neither is legal JSON,
// and accepting either would make the canonical form ambiguous.
func Canonical(raw []byte) ([]byte, error) {
	v := jsontext.Value(raw)
	if !v.IsValid() {
		return nil, errors.New("not valid JSON")
	}
	if err := v.Format(jsontext.ReorderRawObjects(true), jsontext.CanonicalizeRawFloats(true)); err != nil {
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	return []byte(v), nil
}
