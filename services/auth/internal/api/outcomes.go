// Login and refresh outcomes (AU-6), counted as auth_logins_total and
// auth_refresh_total on the metrics listener.

package api

// The result label of both counters. Nothing about the caller is ever a label.
const (
	ResultOK      = "ok"      // tokens were issued
	ResultInvalid = "invalid" // a bad body, or a refresh token that is unknown or expired
	ResultError   = "error"   // the server failed; the client got a 500
	// Refresh only.
	ResultRevoked       = "revoked"        // a logged-out token, or one of a revoked family
	ResultReuseDetected = "reuse_detected" // an exchanged token came back; its family is revoked
)

// Outcomes counts what each login and refresh ended in. The server's metrics implement
// it; nil counts nothing.
type Outcomes interface {
	Login(result string, newAccount bool)
	Refresh(result string)
}

func countLogin(o Outcomes, result string, newAccount bool) {
	if o != nil {
		o.Login(result, newAccount)
	}
}

func countRefresh(o Outcomes, result string) {
	if o != nil {
		o.Refresh(result)
	}
}
