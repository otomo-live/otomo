// Package manifest loads the current release manifest per channel into an immutable
// in-memory set.
//
// Patch reads Config's `channel_head` pointer joined to `release`. The manifest column
// is jsonb, which does not preserve the exact bytes its manifest_sha256 was computed
// over — Postgres stores a parsed form and even orders object keys by length. The
// loader therefore re-derives the canonical bytes with Canonical and refuses to serve a
// channel whose bytes do not hash back to the stored manifest_sha256.
//
// Publication is a single atomic pointer swap. A Set is never mutated after it is
// published, so a handler does one Holder.Load and a map lookup with no lock, and a
// reader that grabbed an older Set keeps a complete, self-consistent snapshot.
package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is one channel's current release, ready to serve. Body holds the canonical
// manifest bytes and is written verbatim, so it must not be mutated.
type Entry struct {
	Channel          string
	ReleaseID        int64
	Body             []byte
	ETag             string
	MinClientVersion string

	// Server is the same release's server manifest (CF-3): the server-audience
	// namespaces, served only on the internal listener. It is loaded and hash-checked
	// from the same row as the client manifest, so the two always describe one release.
	Server Document
}

// Document is a manifest ready to serve: canonical bytes, written verbatim, and the
// quoted ETag derived from their SHA-256.
type Document struct {
	Body []byte
	ETag string
}

// Set is an immutable snapshot of every loaded channel. Build one only through the
// Loader or the package's own constructors; after publish it is shared by every reader
// and must never be written again.
type Set struct {
	entries map[string]*Entry
}

func newSet(entries map[string]*Entry) *Set {
	return &Set{entries: entries}
}

// NewSet returns an immutable Set holding entries. It exists for tests outside this
// package and for any future caller that assembles a set from a source other than
// Postgres; production loading goes through the Loader. The map and entries are not
// copied and must not be mutated once the Set is shared.
func NewSet(entries map[string]*Entry) *Set {
	return newSet(entries)
}

// Get returns the entry for channel and whether one is loaded. A nil Set has nothing,
// which lets a caller that has never had a successful load read through the pointer
// without a nil check.
func (s *Set) Get(channel string) (*Entry, bool) {
	if s == nil {
		return nil, false
	}
	e, ok := s.entries[channel]
	return e, ok
}

// Holder is the lock-free pointer every handler reads. The zero value has no set and
// reports itself unready until the first successful publish.
type Holder struct {
	p atomic.Pointer[Set]
}

// NewHolder returns a Holder already publishing s. It exists for tests outside this
// package and for any future caller that seeds a holder directly; production loading
// goes through the Loader, which publishes a fresh Set per reload.
func NewHolder(s *Set) *Holder {
	h := &Holder{}
	h.p.Store(s)
	return h
}

// Load returns the current Set, or nil if none has been published yet.
func (h *Holder) Load() *Set {
	return h.p.Load()
}

// Ready reports whether a Set has ever been published. The message is the one /readyz
// has always returned for an unloaded Patch.
func (h *Holder) Ready() error {
	if h.p.Load() == nil {
		return errors.New("manifests not loaded")
	}
	return nil
}

// publish swaps in a new immutable set. Callers must not retain or mutate s afterwards.
func (h *Holder) publish(s *Set) {
	h.p.Store(s)
}

// Canonical returns the canonical bytes of a manifest: object members sorted by name,
// no insignificant whitespace and minimal string escapes. Integer literals are kept
// exactly as written — 9007199254740993 survives — while raw floats are shortened the
// way RFC 8785 requires.
//
// It deliberately does not call Value.Canonicalize: that path reinterprets every number
// as a float64, which silently rounds integers above 2^53. Format with
// CanonicalizeRawFloats only touches numbers that already carry a fraction or exponent.
func Canonical(raw []byte) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(false))
	val, err := dec.ReadValue()
	if err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	// val aliases the decoder's read buffer, which the trailing-token check below and
	// any later decode overwrite. Copy before doing anything else with it.
	canonical := jsontext.Value(append([]byte(nil), val...))

	if _, err := dec.ReadToken(); err != io.EOF {
		if err == nil {
			return nil, errors.New("manifest has trailing data after the top-level value")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	if err := canonical.Format(jsontext.ReorderRawObjects(true), jsontext.CanonicalizeRawFloats(true)); err != nil {
		return nil, fmt.Errorf("canonicalize manifest: %w", err)
	}
	return []byte(canonical), nil
}

// querier is the slice of pgx the loader needs. *pgxpool.Pool and pgx.Tx both satisfy
// it, so a test can point the loader at a transaction it will roll back.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Loader reads channel heads from Postgres and publishes a new Set on success.
type Loader struct {
	DB        *pgxpool.Pool
	Log       *slog.Logger
	Holder    *Holder
	OnPublish func(Entry)

	// q overrides DB and exists only so tests can drive the loader with a transaction.
	q querier
}

const selectHeads = `SELECT h.channel, r.release_id, r.manifest::text, r.manifest_sha256, r.min_client_version,
       r.server_manifest::text, r.server_manifest_sha256
  FROM channel_head h
  JOIN release r USING (release_id)`

func (l *Loader) queryer() querier {
	if l.q != nil {
		return l.q
	}
	return l.DB
}

func (l *Loader) logger() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// LoadAll reads every channel in one query and publishes the set of rows that passed
// their hash check. A row that does not hash back to its manifest_sha256 is logged and
// dropped; if no row survives, nothing is published and an error is returned so the
// holder stays at its previous good set (or unloaded, if it never had one).
func (l *Loader) LoadAll(ctx context.Context) error {
	rows, err := l.queryer().Query(ctx, selectHeads)
	if err != nil {
		return fmt.Errorf("query channel heads: %w", err)
	}
	defer rows.Close()

	// Start from what is being served: a channel whose new row fails its hash check
	// keeps its last good entry instead of vanishing, so one bad release cannot take a
	// working channel offline on a reload.
	entries := make(map[string]*Entry)
	good := 0
	if current := l.Holder.Load(); current != nil {
		for name, e := range current.entries {
			entries[name] = e
		}
	}
	rejected := 0
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			rejected++
			l.logger().Error("manifest row rejected", slog.Any("error", err))
			continue
		}
		entries[e.Channel] = e
		good++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read channel heads: %w", err)
	}
	if good == 0 {
		return fmt.Errorf("no manifest passed its hash check (%d rejected)", rejected)
	}
	l.publish(newSet(entries))
	return nil
}

// Reload reads one channel and publishes a new set that is a copy of the current one
// with that channel replaced. A row that fails its hash check leaves the previous entry
// in place: the error is returned and nothing is published.
func (l *Loader) Reload(ctx context.Context, channel string) error {
	rows, err := l.queryer().Query(ctx, selectHeads+` WHERE h.channel = $1`, channel)
	if err != nil {
		return fmt.Errorf("query channel head %q: %w", channel, err)
	}
	defer rows.Close()

	var fresh *Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return err
		}
		fresh = e
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read channel head %q: %w", channel, err)
	}
	if fresh == nil {
		return fmt.Errorf("channel %q has no head", channel)
	}

	entries := make(map[string]*Entry)
	if current := l.Holder.Load(); current != nil {
		for name, e := range current.entries {
			entries[name] = e
		}
	}
	entries[fresh.Channel] = fresh
	l.publish(newSet(entries))
	return nil
}

func scanEntry(rows pgx.Rows) (*Entry, error) {
	var (
		channel   string
		releaseID int64
		raw       string
		sum       string
		minVer    string
		serverRaw *string
		serverSum *string
	)
	if err := rows.Scan(&channel, &releaseID, &raw, &sum, &minVer, &serverRaw, &serverSum); err != nil {
		return nil, fmt.Errorf("scan channel head: %w", err)
	}
	return buildEntry(channel, releaseID, raw, sum, minVer, serverRaw, serverSum)
}

// buildEntry re-derives the canonical bytes and checks them against the stored hash.
// This is the one place the hash check happens, shared by LoadAll and Reload.
//
// The server manifest is checked the same way. A row where either manifest fails its
// check is rejected whole, so the channel keeps serving its last good release rather
// than a client manifest from one release and a server manifest from another. A NULL
// server manifest (a release from before Config stored one) is an empty one.
func buildEntry(channel string, releaseID int64, raw, sum, minVer string, serverRaw, serverSum *string) (*Entry, error) {
	body, err := checked([]byte(raw), sum)
	if err != nil {
		return nil, fmt.Errorf("%s release %d: manifest: %w", channel, releaseID, err)
	}

	var server Document
	switch {
	case serverRaw == nil || serverSum == nil:
		server, err = EmptyServerManifest(body)
		if err != nil {
			return nil, fmt.Errorf("%s release %d: empty server manifest: %w", channel, releaseID, err)
		}
	default:
		serverBody, err := checked([]byte(*serverRaw), *serverSum)
		if err != nil {
			return nil, fmt.Errorf("%s release %d: server manifest: %w", channel, releaseID, err)
		}
		server = Document{Body: serverBody, ETag: `"` + *serverSum + `"`}
	}

	return &Entry{
		Channel:          channel,
		ReleaseID:        releaseID,
		Body:             body,
		ETag:             `"` + sum + `"`,
		MinClientVersion: minVer,
		Server:           server,
	}, nil
}

// checked returns raw's canonical bytes if they hash to sum.
func checked(raw []byte, sum string) ([]byte, error) {
	body, err := Canonical(raw)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(body)
	gotHex := hex.EncodeToString(got[:])
	if !strings.EqualFold(gotHex, sum) {
		return nil, fmt.Errorf("sha256 %s does not match canonical bytes %s", sum, gotHex)
	}
	return body, nil
}

// EmptyServerManifest builds the server manifest of a release that has none stored: the
// client manifest's own fields (format, channel, release_id, created_at,
// min_client_version) with an empty config and no packs. That is exactly what Config
// writes for a release with no server namespaces (design/02-config.md §4), so a caller
// cannot tell the two apart, and it is an ordinary manifest rather than an error.
func EmptyServerManifest(clientBody []byte) (Document, error) {
	var fields map[string]jsontext.Value
	dec := jsontext.NewDecoder(bytes.NewReader(clientBody))
	val, err := dec.ReadValue()
	if err != nil {
		return Document{}, err
	}
	if err := unmarshalObject(val, &fields); err != nil {
		return Document{}, err
	}
	fields["config"] = jsontext.Value(`{}`)
	fields["packs"] = jsontext.Value(`[]`)

	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return Document{}, err
	}
	for name, v := range fields {
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return Document{}, err
		}
		if err := enc.WriteValue(v); err != nil {
			return Document{}, err
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return Document{}, err
	}

	body, err := Canonical(buf.Bytes())
	if err != nil {
		return Document{}, err
	}
	sum := sha256.Sum256(body)
	return Document{Body: body, ETag: `"` + hex.EncodeToString(sum[:]) + `"`}, nil
}

// unmarshalObject splits a JSON object into its members, keeping each value's raw bytes
// so numbers are never reinterpreted.
func unmarshalObject(val jsontext.Value, out *map[string]jsontext.Value) error {
	dec := jsontext.NewDecoder(bytes.NewReader(val))
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '{' {
		return errors.New("manifest is not a JSON object")
	}
	m := map[string]jsontext.Value{}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		// Copy the name out now: the next decoder call voids the token.
		name := tok.String()
		v, err := dec.ReadValue()
		if err != nil {
			return err
		}
		m[name] = append(jsontext.Value(nil), v...)
	}
	*out = m
	return nil
}

func (l *Loader) publish(s *Set) {
	l.Holder.publish(s)
	if l.OnPublish == nil {
		return
	}
	for _, e := range s.entries {
		l.OnPublish(*e)
	}
}
