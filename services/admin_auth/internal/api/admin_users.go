// The staff user-management API, served under /api/admin/users.
//
// Every route is wrapped by the admin middleware in the server package, which has
// already verified the bearer token, loaded the caller from the database and refused
// anyone without the admin role. These handlers read the caller from the request
// context and use it as the audit actor; they never trust a role from the token.
//
// The invite and reset tokens are secrets. Each is 32 random bytes, only its sha256
// reaches the database, and it is returned to the caller exactly once inside a URL
// fragment, which a browser never sends to a server.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/otomo-live/otomo/services/admin_auth/internal/store"
)

// maxAdminBody bounds every user-management request body, matching the login path's
// 16 KiB contract.
const maxAdminBody = 16 << 10

// maxEmailLength is the longest address the API accepts. RFC 5321 caps a path at 254
// octets; the database has no length limit, so this is the only place it is enforced.
const maxEmailLength = 254

// onboardingPath is the admin UI page that consumes both invite and reset links.
const onboardingPath = "/admin/onboard"

// Admin is the authenticated caller put on the request context by the admin
// middleware. It is the audit actor for every mutation.
type Admin struct {
	ID     string
	Name   string
	IsRoot bool
}

// adminKey is the context key for the caller. The unexported struct type keeps any
// importing package from colliding with it.
type adminKey struct{}

// WithAdmin returns a copy of ctx carrying the authenticated admin.
func WithAdmin(ctx context.Context, a Admin) context.Context {
	return context.WithValue(ctx, adminKey{}, a)
}

// AdminFrom returns the admin stored by WithAdmin. The bool is false when the request
// did not pass through the admin middleware, which handlers treat as an internal bug.
func AdminFrom(ctx context.Context) (Admin, bool) {
	a, ok := ctx.Value(adminKey{}).(Admin)
	return a, ok
}

// AdminStore is the subset of *store.DB the user-management handlers need. It is an
// interface for the same reason the session stores are: the server package owns the
// concrete wiring.
type AdminStore interface {
	ListUsers(ctx context.Context) ([]store.AdminUser, error)
	CreateInvite(ctx context.Context, in store.CreateInviteInput, actor store.Entry) (store.Invite, error)
	CreateReset(ctx context.Context, in store.CreateResetInput, actor store.Entry) (store.Invite, error)
	ListInvites(ctx context.Context) ([]store.Invite, error)
	RevokeInvite(ctx context.Context, id string, actor store.Entry) error
	UpdateUser(ctx context.Context, in store.UpdateUserInput, actor store.Entry) (store.AdminUser, error)
	ResetMFA(ctx context.Context, userID string, actor store.Entry) (store.ResetMFAResult, error)
}

// AdminDeps is everything the user-management handlers need.
type AdminDeps struct {
	Store     AdminStore
	InviteTTL time.Duration
	PublicURL string
	Logger    *slog.Logger
}

// adminUserJSON is the account shape every route returns. It has no field that could
// carry password_hash or a TOTP secret.
type adminUserJSON struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Roles       []string   `json:"roles"`
	IsRoot      bool       `json:"is_root"`
	Status      string     `json:"status"`
	MFAEnrolled bool       `json:"mfa_enrolled"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

type adminUsersResponse struct {
	Users []adminUserJSON `json:"users"`
}

type inviteJSON struct {
	ID        string    `json:"id"`
	Purpose   string    `json:"purpose"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      *string   `json:"role"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type invitesResponse struct {
	Invites []inviteJSON `json:"invites"`
}

type inviteResponse struct {
	InviteID  string    `json:"invite_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InviteURL string    `json:"invite_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type resetResponse struct {
	InviteID  string    `json:"invite_id"`
	ResetURL  string    `json:"reset_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type inviteRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

type patchUserRequest struct {
	Roles  *[]string `json:"roles"`
	Status *string   `json:"status"`
}

// validRole reports whether role is one of the three the schema allows.
func validRole(role string) bool {
	switch role {
	case "viewer", "live_ops", "admin":
		return true
	default:
		return false
	}
}

// validEmail applies the endpoint's deliberately light check: one @ and at most 254
// bytes. Anything stricter would reject addresses the database is happy to hold.
func validEmail(email string) bool {
	return len(email) <= maxEmailLength && len(email) >= 3 && strings.Count(email, "@") == 1
}

// buildOnboardURL places the one-time token in the URL fragment. Browsers never send
// the fragment to a server, so the token cannot land in Gateway or nginx access logs
// or leak through a Referer header — unlike a query string, which is logged
// everywhere.
func buildOnboardURL(publicURL, token string) string {
	return strings.TrimRight(publicURL, "/") + onboardingPath + "#token=" + token
}

// newLinkToken returns 32 random bytes as unpadded base64url. Only its sha256 reaches
// the database. It shares the refresh-token generator so both secrets have identical
// entropy and encoding.
func newLinkToken() (string, []byte, error) {
	token, err := newRefreshToken()
	if err != nil {
		return "", nil, err
	}
	hash := sha256.Sum256([]byte(token))
	return token, hash[:], nil
}

func adminLogger(deps AdminDeps) *slog.Logger {
	if deps.Logger != nil {
		return deps.Logger
	}
	return slog.Default()
}

// decodeAdminBody reads the one JSON object the body must contain: at most 16 KiB, no
// unknown fields, and nothing after the object. A violation is a 400 invalid_body and
// the caller must return immediately.
func decodeAdminBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not a single valid JSON object")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		WriteError(w, r, http.StatusBadRequest, "invalid_body", "request body is not a single valid JSON object")
		return false
	}
	return true
}

// adminUserJSONFrom converts a stored account into the wire shape.
func adminUserJSONFrom(u store.AdminUser) adminUserJSON {
	return adminUserJSON{
		ID:          u.ID,
		Email:       u.Email,
		Name:        u.Name,
		Roles:       u.Roles,
		IsRoot:      u.IsRoot,
		Status:      u.Status,
		MFAEnrolled: u.MFAEnrolled,
		LastLoginAt: u.LastLoginAt,
		CreatedAt:   u.CreatedAt,
	}
}

// caller returns the admin from the context, or the zero value. The admin middleware
// guarantees one is present on every route that calls this.
func caller(r *http.Request) Admin {
	a, _ := AdminFrom(r.Context())
	return a
}

// ListAdminUsers returns GET /api/admin/users.
func ListAdminUsers(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := deps.Store.ListUsers(r.Context())
		if err != nil {
			internalError(w, r, log, "list staff users", err)
			return
		}
		out := make([]adminUserJSON, 0, len(users))
		for _, u := range users {
			out = append(out, adminUserJSONFrom(u))
		}
		writeJSON(w, http.StatusOK, adminUsersResponse{Users: out})
	}
}

// InviteUser returns POST /api/admin/users. It creates an invite link; the account
// itself is created later when the invite is consumed.
func InviteUser(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		var req inviteRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		req.Email = strings.TrimSpace(req.Email)
		req.Name = strings.TrimSpace(req.Name)
		req.Role = strings.TrimSpace(req.Role)

		if !validEmail(req.Email) {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "email must contain exactly one @ and be at most 254 characters")
			return
		}
		if n := utf8.RuneCountInString(req.Name); n < 1 || n > 100 {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "name must be between 1 and 100 characters")
			return
		}
		if !validRole(req.Role) {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "role must be one of viewer, live_ops, admin")
			return
		}

		actor := caller(r)
		if req.Role == "admin" && !actor.IsRoot {
			WriteError(w, r, http.StatusForbidden, "insufficient_role", "only root can grant admin")
			return
		}

		token, hash, err := newLinkToken()
		if err != nil {
			internalError(w, r, log, "mint invite token", err)
			return
		}
		inv, err := deps.Store.CreateInvite(r.Context(), store.CreateInviteInput{
			TokenHash: hash,
			Email:     req.Email,
			Name:      req.Name,
			Role:      req.Role,
			TTL:       deps.InviteTTL,
		}, store.Entry{ID: actor.ID, Name: actor.Name})
		switch {
		case errors.Is(err, store.ErrEmailExists):
			WriteError(w, r, http.StatusConflict, "already_exists", "a staff user already has that email")
			return
		case errors.Is(err, store.ErrInvitePending):
			WriteError(w, r, http.StatusConflict, "invite_pending", "a pending invite already exists for that email")
			return
		case err != nil:
			internalError(w, r, log, "create invite", err)
			return
		}

		writeJSON(w, http.StatusCreated, inviteResponse{
			InviteID:  inv.ID,
			Email:     inv.Email,
			Role:      req.Role,
			InviteURL: buildOnboardURL(deps.PublicURL, token),
			ExpiresAt: inv.ExpiresAt,
		})
	}
}

// ListInvites returns GET /api/admin/users/invites.
func ListInvites(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		invites, err := deps.Store.ListInvites(r.Context())
		if err != nil {
			internalError(w, r, log, "list invites", err)
			return
		}
		out := make([]inviteJSON, 0, len(invites))
		for _, inv := range invites {
			out = append(out, inviteJSON{
				ID:        inv.ID,
				Purpose:   inv.Purpose,
				Email:     inv.Email,
				Name:      inv.Name,
				Role:      inv.Role,
				CreatedBy: inv.CreatedBy,
				CreatedAt: inv.CreatedAt,
				ExpiresAt: inv.ExpiresAt,
			})
		}
		writeJSON(w, http.StatusOK, invitesResponse{Invites: out})
	}
}

// RevokeInvite returns DELETE /api/admin/users/invites/{id}.
func RevokeInvite(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := uuid.Parse(id); err != nil {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such invite")
			return
		}
		actor := caller(r)
		err := deps.Store.RevokeInvite(r.Context(), id, store.Entry{ID: actor.ID, Name: actor.Name})
		if errors.Is(err, store.ErrInviteNotFound) {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such pending invite")
			return
		}
		if err != nil {
			internalError(w, r, log, "revoke invite", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// PatchUser returns PATCH /api/admin/users/{id}.
func PatchUser(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		var req patchUserRequest
		if !decodeAdminBody(w, r, &req) {
			return
		}
		if req.Roles == nil && req.Status == nil {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "roles or status is required")
			return
		}
		if req.Roles != nil {
			if len(*req.Roles) == 0 {
				WriteError(w, r, http.StatusBadRequest, "validation_failed", "roles must not be empty")
				return
			}
			for _, role := range *req.Roles {
				if !validRole(role) {
					WriteError(w, r, http.StatusBadRequest, "validation_failed", "roles must be a subset of viewer, live_ops, admin")
					return
				}
			}
		}
		if req.Status != nil && *req.Status != "active" && *req.Status != "disabled" {
			WriteError(w, r, http.StatusBadRequest, "validation_failed", "status must be active or disabled")
			return
		}

		id := r.PathValue("id")
		if _, err := uuid.Parse(id); err != nil {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such user")
			return
		}

		actor := caller(r)
		in := store.UpdateUserInput{
			ID:        id,
			ActorID:   actor.ID,
			ActorRoot: actor.IsRoot,
			HasRoles:  req.Roles != nil,
			HasStatus: req.Status != nil,
		}
		if req.Roles != nil {
			in.Roles = *req.Roles
		}
		if req.Status != nil {
			in.Status = *req.Status
		}

		u, err := deps.Store.UpdateUser(r.Context(), in, store.Entry{ID: actor.ID, Name: actor.Name})
		switch {
		case errors.Is(err, store.ErrNotFound):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such user")
			return
		case errors.Is(err, store.ErrRootProtected):
			WriteError(w, r, http.StatusForbidden, "root_protected", "the root account can only be changed through the CLI")
			return
		case errors.Is(err, store.ErrSelfModification):
			WriteError(w, r, http.StatusForbidden, "self_modification", "you cannot modify your own account")
			return
		case errors.Is(err, store.ErrInsufficientRole):
			WriteError(w, r, http.StatusForbidden, "insufficient_role", "only root can change admin access")
			return
		case err != nil:
			internalError(w, r, log, "update user", err)
			return
		}
		writeJSON(w, http.StatusOK, adminUserJSONFrom(u))
	}
}

// ResetUserPassword returns POST /api/admin/users/{id}/reset. The onboarding page
// handles both invite and reset links, so the URL shape is the same.
func ResetUserPassword(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := uuid.Parse(id); err != nil {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such user")
			return
		}

		token, hash, err := newLinkToken()
		if err != nil {
			internalError(w, r, log, "mint reset token", err)
			return
		}

		actor := caller(r)
		inv, err := deps.Store.CreateReset(r.Context(), store.CreateResetInput{
			TokenHash: hash,
			UserID:    id,
			ActorRoot: actor.IsRoot,
			TTL:       deps.InviteTTL,
		}, store.Entry{ID: actor.ID, Name: actor.Name})
		switch {
		case errors.Is(err, store.ErrNotFound):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such user")
			return
		case errors.Is(err, store.ErrRootProtected):
			WriteError(w, r, http.StatusForbidden, "root_protected", "the root account can only be changed through the CLI")
			return
		case errors.Is(err, store.ErrInsufficientRole):
			WriteError(w, r, http.StatusForbidden, "insufficient_role", "only root can reset an admin password")
			return
		case err != nil:
			internalError(w, r, log, "create reset", err)
			return
		}

		writeJSON(w, http.StatusCreated, resetResponse{
			InviteID:  inv.ID,
			ResetURL:  buildOnboardURL(deps.PublicURL, token),
			ExpiresAt: inv.ExpiresAt,
		})
	}
}

// ResetUserMFA returns POST /api/admin/users/{id}/mfa/reset. It clears the target's
// second factor, recovery codes and open MFA tickets, revokes its live refresh
// sessions and audits mfa.reset, all in one store transaction. The 403 and 409
// responses describe the rule that refused the reset; see ResetMFA for the order.
func ResetUserMFA(deps AdminDeps) http.HandlerFunc {
	log := adminLogger(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := uuid.Parse(id); err != nil {
			WriteError(w, r, http.StatusNotFound, "not_found", "no such user")
			return
		}

		actor := caller(r)
		res, err := deps.Store.ResetMFA(r.Context(), id, store.Entry{ID: actor.ID, Name: actor.Name})
		switch {
		case errors.Is(err, store.ErrNotFound):
			WriteError(w, r, http.StatusNotFound, "not_found", "no such user")
			return
		case errors.Is(err, store.ErrSelfModification):
			WriteError(w, r, http.StatusForbidden, "self_modification", "you cannot reset your own second factor")
			return
		case errors.Is(err, store.ErrRootProtected):
			WriteError(w, r, http.StatusForbidden, "root_protected", "root has no second factor")
			return
		case errors.Is(err, store.ErrInsufficientRole):
			WriteError(w, r, http.StatusForbidden, "insufficient_role", "only root can reset an admin's second factor")
			return
		case errors.Is(err, store.ErrMFANotEnrolled):
			WriteError(w, r, http.StatusConflict, "mfa_not_enrolled", "the account has no second factor to reset")
			return
		case err != nil:
			internalError(w, r, log, "reset user mfa", err)
			return
		}
		writeJSON(w, http.StatusOK, adminUserJSONFrom(res.User))
	}
}
