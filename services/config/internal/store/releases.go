package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
)

// sha256Hex returns the lowercase hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Release is one published manifest snapshot for a channel. Manifest and ServerManifest
// hold the exact canonical bytes that were hashed and stored, so the API can echo them
// without a round trip through Postgres' jsonb text form. ServerManifest is nil and
// ServerManifestSHA256 is "" for a row written before server manifests existed; such a
// row is treated as carrying an empty server manifest.
type Release struct {
	ReleaseID            int64
	Channel              string
	Manifest             json.RawMessage
	ManifestSHA256       string
	ServerManifest       json.RawMessage
	ServerManifestSHA256 string
	MinClientVersion     string
	Message              string
	CreatedBy            string
	CreatedAt            time.Time
}

// PublishVersion pairs a namespace with the immutable version a publish selects.
type PublishVersion struct {
	Namespace string
	Version   int
}

// PublishRequest is what a publish has to resolve. Message and MinClientVersion have
// already been validated by the API layer; the store trusts their shape.
type PublishRequest struct {
	BaseReleaseID    int64
	Versions         []PublishVersion
	PackSHA256       []string
	MinClientVersion string
	Message          string
}

// ErrStaleRelease is returned by Publish when the channel's head is no longer the
// release the caller previewed. It is a sentinel so the API can map it to a 409 without
// inspecting the row it read.
var ErrStaleRelease = errors.New("channel head has moved")

// StaleReleaseError is the concrete form of ErrStaleRelease. It carries both release
// ids so the API's message can name where the channel moved from and to; errors.Is(err,
// ErrStaleRelease) still reports true.
type StaleReleaseError struct {
	Channel string
	Base    int64
	Current int64
}

func (e *StaleReleaseError) Error() string {
	return fmt.Sprintf("channel %s has moved from release %d to %d", e.Channel, e.Base, e.Current)
}

func (e *StaleReleaseError) Unwrap() error { return ErrStaleRelease }

// ErrReleaseTargetUnknown is returned by Publish when a selected version or pack cannot
// be resolved. It is a sentinel so the API can map it to a 404 without inspecting which
// lookup failed.
var ErrReleaseTargetUnknown = errors.New("release target not found")

// UnknownTargetError is the concrete form of ErrReleaseTargetUnknown. Kind is one of
// "namespace", "version" or "pack"; Name identifies the value that did not resolve, so
// the API's 404 can name it.
type UnknownTargetError struct {
	Kind string
	Name string
}

func (e *UnknownTargetError) Error() string {
	return fmt.Sprintf("unknown %s %q", e.Kind, e.Name)
}

func (e *UnknownTargetError) Unwrap() error { return ErrReleaseTargetUnknown }

// blobSize returns the on-disk size of a blob, which is the byte length clients will
// download. A missing blob is a server fault: the release row must never point at
// content that is not there.
func blobSize(blobs *blob.Store, sha string) (int64, error) {
	if blobs == nil {
		return 0, errors.New("no blob store configured")
	}
	rc, size, err := blobs.Open(sha)
	if err != nil {
		return 0, err
	}
	if err := rc.Close(); err != nil {
		return 0, fmt.Errorf("close %s: %w", sha, err)
	}
	return size, nil
}

// manifestConfig is one entry of a client manifest's "config" object.
type manifestConfig struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// manifestPack is one entry of a client manifest's "packs" array.
type manifestPack struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// manifestDoc is §4's manifest. Its field order does not matter because the value is
// canonicalised before it is hashed, but the names and types are the contract.
type manifestDoc struct {
	Format           int                       `json:"format"`
	Channel          string                    `json:"channel"`
	ReleaseID        int64                     `json:"release_id"`
	CreatedAt        string                    `json:"created_at"`
	MinClientVersion string                    `json:"min_client_version"`
	Config           map[string]manifestConfig `json:"config"`
	Packs            []manifestPack            `json:"packs"`
}

// Publish builds a new release for channel and moves the channel head to it, in one
// transaction.
//
// The channel_head row is locked FOR UPDATE first, which serialises publishes per
// channel: a caller that presents a base the channel has moved past gets
// ErrStaleRelease, and only one caller can reach the INSERT for a given base.
//
// Every selected version and pack is then resolved. An unknown namespace or version,
// and an unknown pack, return UnknownTargetError (a 404 at the API layer). A version
// from a server-audience namespace goes into the release's server manifest rather than
// its client manifest, so server-only tuning never reaches a client. Server namespaces
// are therefore allowed on every channel, and both manifests are written for every
// release.
//
// Each resolved item's blob is confirmed to exist and its size is read from disk, so
// the manifest describes the bytes the consumer will actually download. A missing blob
// is a server fault and aborts the transaction.
//
// Both manifests are canonicalised and hashed before the INSERT, and the sequence is
// read with nextval rather than the column default because the manifest states
// release_id. The audit entry and the pg_notify call share the transaction, so the
// notification is delivered only after the release is committed.
func (d *DB) Publish(ctx context.Context, channel string, req PublishRequest, actor Entry, blobs *blob.Store) (Release, error) {
	var rel Release

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var current int64
		err := tx.QueryRow(ctx,
			`SELECT release_id FROM channel_head WHERE channel = $1 FOR UPDATE`,
			channel).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("store: channel %q has no head", channel)
		}
		if err != nil {
			return fmt.Errorf("store: lock channel head: %w", err)
		}
		if current != req.BaseReleaseID {
			return &StaleReleaseError{Channel: channel, Base: req.BaseReleaseID, Current: current}
		}

		clientConfigs := make(map[string]manifestConfig, len(req.Versions))
		serverConfigs := make(map[string]manifestConfig)
		serverVersions := make([]map[string]any, 0, len(req.Versions))
		for _, pv := range req.Versions {
			var audience string
			if err := tx.QueryRow(ctx,
				`SELECT audience FROM config_namespace WHERE name = $1`, pv.Namespace).Scan(&audience); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return &UnknownTargetError{Kind: "namespace", Name: pv.Namespace}
				}
				return fmt.Errorf("store: resolve namespace: %w", err)
			}

			var sha string
			if err := tx.QueryRow(ctx,
				`SELECT sha256 FROM config_version WHERE namespace = $1 AND version = $2`,
				pv.Namespace, pv.Version).Scan(&sha); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return &UnknownTargetError{
						Kind: "version",
						Name: fmt.Sprintf("%s version %d", pv.Namespace, pv.Version),
					}
				}
				return fmt.Errorf("store: resolve version: %w", err)
			}

			size, err := blobSize(blobs, sha)
			if err != nil {
				return fmt.Errorf("store: version %s v%d blob %s: %w", pv.Namespace, pv.Version, sha, err)
			}
			entry := manifestConfig{Version: pv.Version, SHA256: sha, Size: size}
			if audience == "server" {
				serverConfigs[pv.Namespace] = entry
				serverVersions = append(serverVersions, map[string]any{"namespace": pv.Namespace, "version": pv.Version})
			} else {
				clientConfigs[pv.Namespace] = entry
			}
		}

		packs := make([]manifestPack, 0, len(req.PackSHA256))
		for _, want := range req.PackSHA256 {
			var name, sha string
			if err := tx.QueryRow(ctx,
				`SELECT name, sha256 FROM content_pack WHERE sha256 = $1`, want).Scan(&name, &sha); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return &UnknownTargetError{Kind: "pack", Name: want}
				}
				return fmt.Errorf("store: resolve pack: %w", err)
			}

			size, err := blobSize(blobs, sha)
			if err != nil {
				return fmt.Errorf("store: pack %s blob %s: %w", name, sha, err)
			}
			packs = append(packs, manifestPack{Name: name, SHA256: sha, Size: size})
		}
		// §4 fixes the order as sorted by name then sha, so two publishes of the same
		// selection produce the same manifest bytes.
		sort.Slice(packs, func(i, j int) bool {
			if packs[i].Name != packs[j].Name {
				return packs[i].Name < packs[j].Name
			}
			return packs[i].SHA256 < packs[j].SHA256
		})

		id, err := nextReleaseID(ctx, tx)
		if err != nil {
			return err
		}

		// Both manifests share one timestamp, so a release's client and server halves
		// agree on when it was published.
		createdAt := time.Now().UTC().Truncate(time.Second)

		_, canonical, sha, err := buildReleaseManifest(channel, id, createdAt, req.MinClientVersion, clientConfigs, packs)
		if err != nil {
			return err
		}
		_, serverCanonical, serverSHA, err := buildReleaseManifest(channel, id, createdAt, req.MinClientVersion, serverConfigs, []manifestPack{})
		if err != nil {
			return err
		}

		rel, err = insertRelease(ctx, tx, id, channel, req.MinClientVersion, req.Message, actor,
			canonical, sha, serverCanonical, serverSHA)
		if err != nil {
			return err
		}

		if err := moveChannelHead(ctx, tx, channel, id, actor); err != nil {
			return err
		}

		versions := make([]map[string]any, 0, len(req.Versions))
		for _, pv := range req.Versions {
			versions = append(versions, map[string]any{"namespace": pv.Namespace, "version": pv.Version})
		}
		audit := actor
		audit.Action = "release.publish"
		audit.Target = channel
		audit.Details = map[string]any{
			"release_id":      id,
			"base_release_id": req.BaseReleaseID,
			"versions":        versions,
			"server_versions": serverVersions,
			"packs":           req.PackSHA256,
		}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}

		if err := notifyChannel(ctx, tx, channel); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Release{}, err
	}
	return rel, nil
}

// nextReleaseID allocates the next release id from the table's sequence. The manifest
// states release_id, so the id has to be known before the row is inserted; the column's
// own default would be too late.
func nextReleaseID(ctx context.Context, tx pgx.Tx) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx,
		`SELECT nextval(pg_get_serial_sequence('release','release_id'))`).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: next release id: %w", err)
	}
	return id, nil
}

// buildReleaseManifest is the one place a release manifest's canonical bytes and hash
// are computed. Publish and Promote both go through it — once for the client manifest
// and once for the server manifest — so a promoted manifest can never hash differently
// from the same content published directly. createdAt is a parameter rather than a call
// to time.Now so a release's two manifests carry the exact same timestamp.
func buildReleaseManifest(channel string, id int64, createdAt time.Time, minClientVersion string,
	configs map[string]manifestConfig, packs []manifestPack) (manifestDoc, []byte, string, error) {
	manifest := manifestDoc{
		Format:           1,
		Channel:          channel,
		ReleaseID:        id,
		CreatedAt:        createdAt.Format(time.RFC3339),
		MinClientVersion: minClientVersion,
		Config:           configs,
		Packs:            packs,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return manifestDoc{}, nil, "", fmt.Errorf("store: marshal manifest: %w", err)
	}
	canonical, err := schema.Canonical(raw)
	if err != nil {
		return manifestDoc{}, nil, "", fmt.Errorf("store: canonicalize manifest: %w", err)
	}
	return manifest, canonical, sha256Hex(canonical), nil
}

// serverConfig decodes the config map from a stored server manifest. A nil document — a
// release written before server manifests existed — is treated as an empty map, the same
// shape a release with no server namespaces stores, so the two compare equal and a
// promotion from a legacy release duplicates an empty server config rather than NULL.
func serverConfig(raw *[]byte) (map[string]manifestConfig, error) {
	if raw == nil {
		return map[string]manifestConfig{}, nil
	}
	var doc manifestDoc
	if err := json.Unmarshal(*raw, &doc); err != nil {
		return nil, err
	}
	if doc.Config == nil {
		return map[string]manifestConfig{}, nil
	}
	return doc.Config, nil
}

// insertRelease writes a release row from two already built and hashed manifests,
// inside the caller's transaction, and returns it in the same shape Publish hands back.
func insertRelease(ctx context.Context, tx pgx.Tx, id int64, channel, minClientVersion, message string,
	actor Entry, canonical []byte, sha string, serverCanonical []byte, serverSHA string) (Release, error) {
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`INSERT INTO release (release_id, channel, manifest, manifest_sha256,
		                      server_manifest, server_manifest_sha256,
		                      min_client_version, message, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING created_at`,
		id, channel, canonical, sha, serverCanonical, serverSHA,
		minClientVersion, message, actor.ActorID).Scan(&createdAt); err != nil {
		return Release{}, fmt.Errorf("store: insert release: %w", err)
	}
	return Release{
		ReleaseID:            id,
		Channel:              channel,
		Manifest:             canonical,
		ManifestSHA256:       sha,
		ServerManifest:       serverCanonical,
		ServerManifestSHA256: serverSHA,
		MinClientVersion:     minClientVersion,
		Message:              message,
		CreatedBy:            actor.ActorID,
		CreatedAt:            createdAt,
	}, nil
}

// moveChannelHead points channel at id and records who moved it, inside the caller's
// transaction and behind the FOR UPDATE lock that transaction already holds.
func moveChannelHead(ctx context.Context, tx pgx.Tx, channel string, id int64, actor Entry) error {
	if _, err := tx.Exec(ctx,
		`UPDATE channel_head
		    SET release_id = $2, updated_by = $3, updated_at = now()
		  WHERE channel = $1`,
		channel, id, actor.ActorID); err != nil {
		return fmt.Errorf("store: move channel head: %w", err)
	}
	return nil
}

// notifyChannel asks Patch to refetch a channel once the transaction commits. Postgres
// delivers the notification only after the commit, which is the ordering the channel
// cache depends on.
func notifyChannel(ctx context.Context, tx pgx.Tx, channel string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_notify('config_release', $1)`, channel); err != nil {
		return fmt.Errorf("store: notify config_release: %w", err)
	}
	return nil
}

// ErrNoReleaseChanges is returned by Rollback and Promote when the requested operation
// would leave the channel head exactly as it already is. It is distinct from the
// draft-level ErrNoChanges because the two describe different subjects.
var ErrNoReleaseChanges = errors.New("release would not change the channel head")

// ErrChannelNotFound is returned when a channel has no channel_head row. Every channel
// seeded by the migration has one, so it means an unknown channel.
var ErrChannelNotFound = errors.New("channel not found")

// ErrReleaseNotInChannel is returned by Rollback when the named release exists but
// belongs to some other channel. A release can only be rolled back to on its own
// channel.
var ErrReleaseNotInChannel = errors.New("release is not a release of this channel")

// ReleaseNotInChannelError is the concrete form of ErrReleaseNotInChannel. It names the
// release and the channel so the API's 404 can be specific.
type ReleaseNotInChannelError struct {
	ReleaseID int64
	Channel   string
}

func (e *ReleaseNotInChannelError) Error() string {
	return fmt.Sprintf("release %d is not a release of channel %s", e.ReleaseID, e.Channel)
}

func (e *ReleaseNotInChannelError) Unwrap() error { return ErrReleaseNotInChannel }

// ReleaseSummary is one row of a channel's release history. It deliberately omits the
// manifest bodies: the history endpoint lists up to 200 rows and only the hashes and
// metadata are needed. ServerManifestSHA256 is "" for a row written before server
// manifests existed.
type ReleaseSummary struct {
	ReleaseID            int64
	ManifestSHA256       string
	ServerManifestSHA256 string
	MinClientVersion     string
	Message              string
	CreatedBy            string
	CreatedAt            time.Time
	IsHead               bool
}

// ListReleases returns a channel's releases newest first, at most limit of them, the
// channel's current head id and the next page's cursor when more rows exist. before,
// when non-nil, restricts the page to releases below it.
//
// An unknown channel returns ErrChannelNotFound, which the API maps onto a 400 because
// the channel set is fixed.
func (d *DB) ListReleases(ctx context.Context, channel string, before *int64, limit int) (int64, []ReleaseSummary, *int64, error) {
	var head int64
	err := d.Pool.QueryRow(ctx,
		`SELECT release_id FROM channel_head WHERE channel = $1`, channel).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, ErrChannelNotFound
	}
	if err != nil {
		return 0, nil, nil, fmt.Errorf("store: read channel head: %w", err)
	}

	// One extra row tells a full page from the last page; it is not returned.
	rows, err := d.Pool.Query(ctx,
		`SELECT release_id, manifest_sha256, server_manifest_sha256,
		        min_client_version, message, created_by, created_at
		   FROM release
		  WHERE channel = $1 AND ($2::bigint IS NULL OR release_id < $2)
		  ORDER BY release_id DESC
		  LIMIT $3`, channel, before, limit+1)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("store: list releases: %w", err)
	}
	defer rows.Close()

	releases := make([]ReleaseSummary, 0, limit)
	for rows.Next() {
		var (
			s         ReleaseSummary
			serverSHA *string
		)
		if err := rows.Scan(&s.ReleaseID, &s.ManifestSHA256, &serverSHA, &s.MinClientVersion,
			&s.Message, &s.CreatedBy, &s.CreatedAt); err != nil {
			return 0, nil, nil, fmt.Errorf("store: scan release: %w", err)
		}
		if serverSHA != nil {
			s.ServerManifestSHA256 = *serverSHA
		}
		s.IsHead = s.ReleaseID == head
		releases = append(releases, s)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, nil, fmt.Errorf("store: list releases: %w", err)
	}

	var next *int64
	if len(releases) > limit {
		releases = releases[:limit]
		last := releases[len(releases)-1].ReleaseID
		next = &last
	}
	return head, releases, next, nil
}

// readReleaseTx loads one release and canonicalises its stored manifests so the bytes
// the API echoes hash to the stored hashes exactly, regardless of how Postgres rendered
// the jsonb text. A NULL server manifest (a row written before server manifests existed)
// stays nil.
func readReleaseTx(ctx context.Context, tx pgx.Tx, id int64) (Release, error) {
	var (
		rel            Release
		manifest       []byte
		serverManifest *[]byte
		serverSHA      *string
	)
	err := tx.QueryRow(ctx,
		`SELECT release_id, channel, manifest, manifest_sha256, server_manifest,
		        server_manifest_sha256, min_client_version, message, created_by, created_at
		   FROM release WHERE release_id = $1`, id).Scan(
		&rel.ReleaseID, &rel.Channel, &manifest, &rel.ManifestSHA256, &serverManifest,
		&serverSHA, &rel.MinClientVersion, &rel.Message, &rel.CreatedBy, &rel.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Release{}, pgx.ErrNoRows
		}
		return Release{}, fmt.Errorf("store: read release: %w", err)
	}
	canonical, err := schema.Canonical(manifest)
	if err != nil {
		return Release{}, fmt.Errorf("store: canonicalize stored manifest: %w", err)
	}
	rel.Manifest = canonical
	if serverManifest != nil {
		serverCanonical, err := schema.Canonical(*serverManifest)
		if err != nil {
			return Release{}, fmt.Errorf("store: canonicalize stored server manifest: %w", err)
		}
		rel.ServerManifest = serverCanonical
		if serverSHA != nil {
			rel.ServerManifestSHA256 = *serverSHA
		}
	}
	return rel, nil
}

// Rollback moves a channel's head pointer to an earlier release of the same channel, in
// one transaction. It does not write a release row: rollback changes which snapshot is
// current, it does not create a snapshot.
//
// The channel_head row is locked FOR UPDATE first, so a caller whose base is no longer
// the head gets StaleReleaseError. releaseID must belong to channel, or the call returns
// ReleaseNotInChannelError. Rolling back to the current head returns ErrNoReleaseChanges.
func (d *DB) Rollback(ctx context.Context, channel string, releaseID, baseReleaseID int64, actor Entry) (Release, error) {
	var rel Release

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var current int64
		err := tx.QueryRow(ctx,
			`SELECT release_id FROM channel_head WHERE channel = $1 FOR UPDATE`,
			channel).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChannelNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock channel head: %w", err)
		}
		if current != baseReleaseID {
			return &StaleReleaseError{Channel: channel, Base: baseReleaseID, Current: current}
		}

		target, err := readReleaseTx(ctx, tx, releaseID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ReleaseNotInChannelError{ReleaseID: releaseID, Channel: channel}
		}
		if err != nil {
			return err
		}
		if target.Channel != channel {
			return &ReleaseNotInChannelError{ReleaseID: releaseID, Channel: channel}
		}
		if releaseID == current {
			return ErrNoReleaseChanges
		}

		if err := moveChannelHead(ctx, tx, channel, releaseID, actor); err != nil {
			return err
		}

		audit := actor
		audit.Action = "release.rollback"
		audit.Target = channel
		audit.Details = map[string]any{"from": current, "to": releaseID}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}

		if err := notifyChannel(ctx, tx, channel); err != nil {
			return err
		}
		rel = target
		return nil
	})
	if err != nil {
		return Release{}, err
	}
	return rel, nil
}

// Promote copies the current head release of from onto channel as a new release, in one
// transaction. The new release keeps from's config map, packs and min_client_version;
// only channel, release_id and created_at differ, so the promoted content is byte-for-
// byte the content that was validated on the lower channel.
//
// channel's head is locked FOR UPDATE and from's is locked FOR SHARE, so a promotion
// reads a stable source and cannot race a publish to the target. A base that is no
// longer channel's head is StaleReleaseError. If the new content equals channel's
// current head content, the call returns ErrNoReleaseChanges.
func (d *DB) Promote(ctx context.Context, channel, from string, baseReleaseID int64, message string, actor Entry) (Release, error) {
	var rel Release

	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var current int64
		err := tx.QueryRow(ctx,
			`SELECT release_id FROM channel_head WHERE channel = $1 FOR UPDATE`,
			channel).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChannelNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock channel head: %w", err)
		}
		if current != baseReleaseID {
			return &StaleReleaseError{Channel: channel, Base: baseReleaseID, Current: current}
		}

		var sourceID int64
		err = tx.QueryRow(ctx,
			`SELECT release_id FROM channel_head WHERE channel = $1 FOR SHARE`,
			from).Scan(&sourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChannelNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock source channel head: %w", err)
		}

		var (
			sourceRaw       []byte
			sourceServerRaw *[]byte
		)
		if err := tx.QueryRow(ctx,
			`SELECT manifest, server_manifest FROM release WHERE release_id = $1`, sourceID).Scan(&sourceRaw, &sourceServerRaw); err != nil {
			return fmt.Errorf("store: read source release: %w", err)
		}
		var source manifestDoc
		if err := json.Unmarshal(sourceRaw, &source); err != nil {
			return fmt.Errorf("store: decode source manifest: %w", err)
		}
		sourceServer, err := serverConfig(sourceServerRaw)
		if err != nil {
			return fmt.Errorf("store: decode source server manifest: %w", err)
		}

		// §4 compares the content that determines what consumers receive: the client
		// config map, the server config map, the packs and the minimum client version.
		// release_id, channel and created_at always differ, so comparing the whole
		// manifest would never be equal.
		var (
			headRaw       []byte
			headServerRaw *[]byte
		)
		if err := tx.QueryRow(ctx,
			`SELECT manifest, server_manifest FROM release WHERE release_id = $1`, current).Scan(&headRaw, &headServerRaw); err != nil {
			return fmt.Errorf("store: read head release: %w", err)
		}
		var head manifestDoc
		if err := json.Unmarshal(headRaw, &head); err != nil {
			return fmt.Errorf("store: decode head manifest: %w", err)
		}
		headServer, err := serverConfig(headServerRaw)
		if err != nil {
			return fmt.Errorf("store: decode head server manifest: %w", err)
		}
		if head.MinClientVersion == source.MinClientVersion &&
			reflect.DeepEqual(head.Config, source.Config) &&
			reflect.DeepEqual(head.Packs, source.Packs) &&
			reflect.DeepEqual(headServer, sourceServer) {
			return ErrNoReleaseChanges
		}

		id, err := nextReleaseID(ctx, tx)
		if err != nil {
			return err
		}

		// Both new manifests share one timestamp, exactly as Publish does it.
		createdAt := time.Now().UTC().Truncate(time.Second)
		_, canonical, sha, err := buildReleaseManifest(channel, id, createdAt, source.MinClientVersion, source.Config, source.Packs)
		if err != nil {
			return err
		}
		_, serverCanonical, serverSHA, err := buildReleaseManifest(channel, id, createdAt, source.MinClientVersion, sourceServer, []manifestPack{})
		if err != nil {
			return err
		}

		if message == "" {
			message = fmt.Sprintf("promote %s release %d", from, sourceID)
		}
		rel, err = insertRelease(ctx, tx, id, channel, source.MinClientVersion, message, actor,
			canonical, sha, serverCanonical, serverSHA)
		if err != nil {
			return err
		}

		if err := moveChannelHead(ctx, tx, channel, id, actor); err != nil {
			return err
		}

		audit := actor
		audit.Action = "release.promote"
		audit.Target = channel
		audit.Details = map[string]any{
			"from_channel":      from,
			"source_release_id": sourceID,
			"release_id":        id,
		}
		if err := WriteAudit(ctx, tx, audit); err != nil {
			return err
		}

		if err := notifyChannel(ctx, tx, channel); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Release{}, err
	}
	return rel, nil
}
