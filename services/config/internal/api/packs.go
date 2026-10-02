// Content-pack upload and listing: design/02-config.md §5 rows POST and GET /packs.
//
// A pack is uploaded as its raw .pck bytes, not as JSON, so this handler is the one
// place in the service that streams a request body instead of decoding it. The bytes
// go straight into blob.Store.Put; the only thing read into memory is the four-byte
// magic every Godot pack starts with.

package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/auth"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// packsPath is registered in both the route table and Handlers.implemented; it is a
// constant so the two cannot drift.
const packsPath = "/api/admin/config/packs"

// packMagic is the four-byte header every Godot .pck begins with. It is checked before
// anything reaches the blob store, so a body that is not a pack never becomes a file.
const packMagic = "GDPC"

// maxPackBytesDefault mirrors CONFIG_MAX_PACK_BYTES's default when a handler is built
// with no configured cap (a test, or a half-wired server).
const maxPackBytesDefault = 536870912 // 512 MiB

// packNameRE is the name grammar from §5: a lowercase letter, then up to 63 more
// lowercase letters, digits or underscores.
var packNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// PackStore is the subset of store.DB the pack handlers use. Keeping it an interface
// lets a handler test exercise the upload path without a database.
type PackStore interface {
	CreatePack(ctx context.Context, name string, ref blob.Ref, actor store.Entry) (store.Pack, bool, error)
	ListPacks(ctx context.Context) ([]store.Pack, error)
}

// packResponse is the wire shape for one pack. Field order matches the §5 example, and
// size is the byte length of the blob the SHA-256 was computed over.
type packResponse struct {
	PackID     string    `json:"pack_id"`
	Name       string    `json:"name"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	UploadedBy string    `json:"uploaded_by"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// packResponseFrom maps a store row onto the wire shape.
func packResponseFrom(p store.Pack) packResponse {
	return packResponse{
		PackID:     p.PackID,
		Name:       p.Name,
		SHA256:     p.SHA256,
		Size:       p.Size,
		UploadedBy: p.UploadedBy,
		UploadedAt: p.UploadedAt,
	}
}

// packListResponse wraps the list. Packs is always an array, empty included.
type packListResponse struct {
	Packs []packResponse `json:"packs"`
}

// createPack serves POST /packs. Auth has already established the caller is live_ops.
//
// The request's read and write deadlines are cleared first. Config's server has a
// CONFIG_READ_TIMEOUT of 10s, which is a sensible budget for a JSON request and far
// too short for a 512 MiB upload that a slow client may take minutes to send; without
// this a large pack that got through Gateway would die here.
func (h *Handlers) createPack(w http.ResponseWriter, r *http.Request) {
	clearRequestDeadlines(w)

	name := r.URL.Query().Get("name")
	if !packNameRE.MatchString(name) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			"name must match "+packNameRE.String())
		return
	}

	body := http.MaxBytesReader(w, r.Body, h.maxPackBytes())
	br := bufio.NewReaderSize(body, 64<<10)

	// Peek, never Read: the header bytes stay in the bufio reader and are the first
	// four bytes blob.Put streams to disk. A body that is not a Godot pack is rejected
	// before a temp file can be created.
	head, err := br.Peek(len(packMagic))
	if err != nil || !bytes.Equal(head, []byte(packMagic)) {
		WriteError(w, r, http.StatusBadRequest, "validation_failed",
			"not a Godot content pack (missing GDPC header)")
		return
	}

	packs := h.packStore()
	if h.Blobs == nil {
		h.internalError(w, r, errors.New("no blob store configured"))
		return
	}
	if packs == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	ref, err := h.Blobs.Put(r.Context(), br)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return
		}
		h.internalError(w, r, err)
		return
	}

	var actor store.Entry
	if claims := auth.ClaimsFrom(r.Context()); claims != nil {
		actor = store.Entry{ActorID: claims.Subject, ActorName: claims.ActorName()}
	}

	pack, created, err := packs.CreatePack(r.Context(), name, ref, actor)
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	if h.PackUpload != nil {
		h.PackUpload(ref.Size)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, packResponseFrom(pack))
}

// listPacks serves GET /packs.
func (h *Handlers) listPacks(w http.ResponseWriter, r *http.Request) {
	packs := h.packStore()
	if packs == nil {
		h.internalError(w, r, errors.New("no store configured"))
		return
	}

	list, err := packs.ListPacks(r.Context())
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	items := make([]packResponse, 0, len(list))
	for _, p := range list {
		items = append(items, packResponseFrom(p))
	}
	writeJSON(w, http.StatusOK, packListResponse{Packs: items})
}

// packStore returns the pack store the handlers should use: an explicit test fake when
// one is set, otherwise the shared database handle. A nil *store.DB must not be
// returned as a non-nil interface, because the caller tests the result for nil.
func (h *Handlers) packStore() PackStore {
	if h.Packs != nil {
		return h.Packs
	}
	if h.Store == nil {
		return nil
	}
	return h.Store
}

// maxPackBytes is the configured cap, or the §7 default when none is set.
func (h *Handlers) maxPackBytes() int64 {
	if h.MaxPackBytes > 0 {
		return h.MaxPackBytes
	}
	return maxPackBytesDefault
}

// clearRequestDeadlines removes the server's per-request read and write deadlines so a
// large, slow pack upload is not cut off mid-stream. A ResponseWriter that does not
// support deadline control (httptest, for one) returns ErrNotSupported; that is not an
// error here, because there is no deadline to clear.
func clearRequestDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}
