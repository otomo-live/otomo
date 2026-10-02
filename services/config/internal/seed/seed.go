// Package seed owns the first-run Config namespaces that ship inside the binary.
//
// A namespace is three files: namespace.json (audience and description), schema.json
// (a JSON Schema draft 2020-12 document) and draft.json (a document that validates
// against that schema). Load parses and checks all three; Apply writes them through the
// ordinary store API, so the seeded rows carry the same audit trail as a human's.
//
// Apply never overwrites a namespace that already exists. Seeding is a starting point,
// and a project's edited draft is the work the service exists to preserve — so a
// namespace that is already present is skipped whatever its current schema or draft
// say. That makes `config seed` safe to run against a live database.
package seed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"

	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// namespaceNameRE mirrors the API's slug rule in internal/api/namespaces.go. It is
// duplicated rather than exported so a seed cannot introduce a name the create
// endpoint would reject.
var namespaceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// Audience namespaces seeded by this package may declare.
const (
	audienceClient = "client"
	audienceServer = "server"
)

// Actor is who every seeded change is attributed to. A fixed, self-describing pair
// rather than an empty one, because the audit trail has to name who did it and "seed"
// is the honest answer.
var Actor = store.Entry{ActorID: "seed", ActorName: "seed"}

// Namespace is one parsed seed folder: the namespace's metadata plus the raw schema and
// draft documents, ready to hand to the store.
type Namespace struct {
	Name        string
	Audience    string
	Description string
	Schema      json.RawMessage
	Draft       json.RawMessage
}

// Action is what Apply did to one namespace.
type Action string

const (
	ActionCreated Action = "created" // the namespace did not exist and was seeded
	ActionSkipped Action = "skipped" // the namespace already existed and was left alone
)

// Result is one namespace's outcome. Namespace is the JSON field name so the report
// logs read naturally.
type Result struct {
	Name   string `json:"namespace"`
	Action Action `json:"action"`
}

// Report is the ordered outcome of an Apply or Plan, one Result per namespace.
type Report struct {
	Results []Result
}

// Created returns how many namespaces the run created.
func (r Report) Created() int { return r.count(ActionCreated) }

// Skipped returns how many namespaces the run left alone.
func (r Report) Skipped() int { return r.count(ActionSkipped) }

func (r Report) count(a Action) int {
	n := 0
	for _, result := range r.Results {
		if result.Action == a {
			n++
		}
	}
	return n
}

// Load parses every namespace folder under fsys and checks each one completely.
//
// Every failure names the file it came from, because the operator sees only the seed
// tree and needs to know which of its three files to fix. Directories are visited in
// lexical order, so a report is deterministic.
func Load(fsys fs.FS) ([]Namespace, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("seed: read seed folder: %w", err)
	}

	namespaces := make([]Namespace, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ns, err := loadOne(fsys, entry.Name())
		if err != nil {
			return nil, err
		}
		namespaces = append(namespaces, ns)
	}
	if len(namespaces) == 0 {
		return nil, errors.New("seed: the seed folder contains no namespace directories")
	}
	return namespaces, nil
}

// loadOne reads and checks one namespace directory. Checks run cheapest first: the
// metadata and its name and audience, then schema compilation, then draft validation.
func loadOne(fsys fs.FS, name string) (Namespace, error) {
	metaPath := path.Join(name, "namespace.json")
	raw, err := fs.ReadFile(fsys, metaPath)
	if err != nil {
		return Namespace{}, fmt.Errorf("seed: %s: %w", metaPath, err)
	}
	var meta struct {
		Audience    string `json:"audience"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Namespace{}, fmt.Errorf("seed: %s: %w", metaPath, err)
	}
	if !namespaceNameRE.MatchString(name) {
		return Namespace{}, fmt.Errorf("seed: %s: namespace name %q must match %s", metaPath, name, namespaceNameRE)
	}
	if meta.Audience != audienceClient && meta.Audience != audienceServer {
		return Namespace{}, fmt.Errorf("seed: %s: audience %q must be %q or %q",
			metaPath, meta.Audience, audienceClient, audienceServer)
	}

	schemaPath := path.Join(name, "schema.json")
	schemaRaw, err := fs.ReadFile(fsys, schemaPath)
	if err != nil {
		return Namespace{}, fmt.Errorf("seed: %s: %w", schemaPath, err)
	}
	compiled, err := schema.Compile(schemaRaw)
	if err != nil {
		return Namespace{}, fmt.Errorf("seed: %s: %w", schemaPath, err)
	}

	draftPath := path.Join(name, "draft.json")
	draftRaw, err := fs.ReadFile(fsys, draftPath)
	if err != nil {
		return Namespace{}, fmt.Errorf("seed: %s: %w", draftPath, err)
	}
	issues, err := schema.Validate(compiled, draftRaw)
	if err != nil {
		return Namespace{}, fmt.Errorf("seed: %s: %w", draftPath, err)
	}
	if len(issues) > 0 {
		return Namespace{}, fmt.Errorf("seed: %s: draft does not validate against %s: %s: %s",
			draftPath, schemaPath, issues[0].Pointer, issues[0].Message)
	}

	return Namespace{
		Name:        name,
		Audience:    meta.Audience,
		Description: meta.Description,
		Schema:      json.RawMessage(schemaRaw),
		Draft:       json.RawMessage(draftRaw),
	}, nil
}

// Apply seeds namespaces into db, in order, attributing every write to actor.
//
// A namespace that does not exist is created and then given the seed schema (as the
// next version above CreateNamespace's empty v1) and the seed draft. A namespace that
// does exist is skipped and never written to. The first error stops the run; the
// returned Report says what completed before it.
func Apply(ctx context.Context, db *store.DB, namespaces []Namespace, actor store.Entry) (Report, error) {
	return apply(ctx, db, namespaces, actor, false)
}

// Plan is Apply's read-only twin: it reports what Apply would create without writing
// anything. It exists for `config seed --dry-run`, and it uses the same existence check
// so the two cannot disagree about what counts as present.
func Plan(ctx context.Context, db *store.DB, namespaces []Namespace) (Report, error) {
	return apply(ctx, db, namespaces, store.Entry{}, true)
}

func apply(ctx context.Context, db *store.DB, namespaces []Namespace, actor store.Entry, dryRun bool) (Report, error) {
	var report Report
	for _, ns := range namespaces {
		action, err := applyOne(ctx, db, ns, actor, dryRun)
		if err != nil {
			return report, fmt.Errorf("seed: %s: %w", ns.Name, err)
		}
		report.Results = append(report.Results, Result{Name: ns.Name, Action: action})
	}
	return report, nil
}

// applyOne decides one namespace's fate and, unless dryRun, carries it out.
//
// Existence is probed with LatestSchema: CreateNamespace always writes schema v1, so a
// namespace with no schema row does not exist, and the sentinel distinguishes that from
// a real read failure. CreateNamespace itself is the authoritative race check: if it
// returns ErrNamespaceExists because another run won, that is a skip, not a failure.
func applyOne(ctx context.Context, db *store.DB, ns Namespace, actor store.Entry, dryRun bool) (Action, error) {
	// CreateNamespace writes schema v1, and a namespace that is fresh enough to seed is
	// still at v1; carry that observed version as the precondition so a concurrent
	// writer cannot be overwritten.
	expected := 1
	existing, err := db.LatestSchema(ctx, ns.Name)
	switch {
	case err == nil:
		// A namespace this command created but did not finish (the process died
		// between CreateNamespace and SaveDraft) still has schema v1 {} and a draft
		// at its first revision. Nobody has edited it, so finishing the seed is not
		// overwriting anyone's work; anything else is a human's namespace.
		expected = existing.SchemaVersion
		fresh, err := untouched(ctx, db, ns.Name, existing)
		if err != nil {
			return "", err
		}
		if !fresh {
			return ActionSkipped, nil
		}
		if dryRun {
			return ActionCreated, nil
		}
	case !errors.Is(err, store.ErrNamespaceNotFound):
		return "", err
	case dryRun:
		return ActionCreated, nil
	default:
		if _, err := db.CreateNamespace(ctx, ns.Name, ns.Audience, ns.Description, actor); err != nil {
			if errors.Is(err, store.ErrNamespaceExists) {
				return ActionSkipped, nil
			}
			return "", err
		}
	}

	if _, err := db.ReplaceSchema(ctx, ns.Name, ns.Schema, &expected, actor); err != nil {
		return "", err
	}

	// Read the revision rather than assuming CreateNamespace's 1, so the optimistic
	// lock SaveDraft checks is the one actually on the row.
	draft, err := db.Draft(ctx, ns.Name)
	if err != nil {
		return "", err
	}
	if _, err := db.SaveDraft(ctx, ns.Name, ns.Draft, draft.Revision, actor); err != nil {
		return "", err
	}
	return ActionCreated, nil
}

// untouched reports whether a namespace is exactly as CreateNamespace left it: schema
// v1 with an empty body and the draft still at revision 1.
func untouched(ctx context.Context, db *store.DB, name string, latest store.Schema) (bool, error) {
	if latest.SchemaVersion != 1 || !emptyObject(latest.Body) {
		return false, nil
	}
	draft, err := db.Draft(ctx, name)
	if err != nil {
		return false, err
	}
	return draft.Revision == 1, nil
}

// emptyObject reports whether body is the JSON object {} (in any spacing).
func emptyObject(body []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(body, &m) == nil && m != nil && len(m) == 0
}
