// Refresh-cookie helpers shared by login, refresh and logout. The cookie's
// attributes are fixed by the identity contract (06 §13.2), so they live in exactly
// one place and the cleared form is the same cookie with an empty value and
// MaxAge -1.

package api

import "net/http"

// setRefreshCookie writes the refresh cookie with the contract's attributes:
// host-scoped by the __Host- prefix (Path=/, Secure, no Domain), unreadable from
// JavaScript, and Strict so a cross-site form cannot carry it. maxAge is the cookie
// lifetime in seconds; pass -1 to clear.
func setRefreshCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		// Domain must never be set: __Host- is host-only and rejects any Domain.
		Name:     refreshCookieName,
		Value:    value,
		Path:     refreshCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearRefreshCookie expires the refresh cookie. It is the same cookie with the same
// attributes, so a browser matches it and drops the stored value; the value is
// deliberately empty rather than omitted.
func clearRefreshCookie(w http.ResponseWriter) {
	setRefreshCookie(w, "", -1)
}
