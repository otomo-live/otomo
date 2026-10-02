// Password policy for the onboarding path: length, identity and a small embedded
// denylist of well-known passwords. It is separate from hashing because the rules are
// checked before a hash is ever computed.
//
// The denylist lives in common.txt and is embedded so the binary needs no external
// file at runtime. It is deliberately short (~250 entries): it stops the passwords
// every credential-stuffing list starts with, not every password a determined user
// might pick.

package password

import (
	_ "embed"
	"errors"
	"strings"
	"unicode/utf8"
)

// commonPasswords is the embedded denylist, one password per line. Blank lines and
// surrounding whitespace are ignored; entries are compared case-insensitively.
//
//go:embed common.txt
var commonPasswords string

// commonSet is the denylist indexed by its lowercase form, built once at startup.
var commonSet = buildCommonSet(commonPasswords)

// The policy errors. Their messages are shown to the caller verbatim by the
// onboarding handler, and each names the rule it was refused for.
var (
	// ErrPasswordTooShort: fewer than 12 runes.
	ErrPasswordTooShort = errors.New("password must be at least 12 characters")
	// ErrPasswordTooLong: more than 128 runes.
	ErrPasswordTooLong = errors.New("password must be at most 128 characters")
	// ErrPasswordIdentity: equal, case-insensitively, to the email or its local part.
	ErrPasswordIdentity = errors.New("password must not be the email address or its local part")
	// ErrPasswordCommon: present in the embedded denylist.
	ErrPasswordCommon = errors.New("password is too common")
)

// MinRunes and MaxRunes bound the accepted password length. They are runes, not
// bytes, so a multi-byte character counts once.
const (
	MinRunes = 12
	MaxRunes = 128
)

// buildCommonSet turns the embedded list into a lookup set. A line longer than any
// password the policy accepts can never match a valid password, so it is skipped.
func buildCommonSet(list string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, line := range strings.Split(list, "\n") {
		entry := strings.ToLower(strings.TrimSpace(line))
		if entry == "" || utf8.RuneCountInString(entry) > MaxRunes {
			continue
		}
		set[entry] = struct{}{}
	}
	return set
}

// Validate applies the onboarding password policy and returns the first rule the
// password breaks, or nil. email is the address the link is for and is matched
// case-insensitively along with its local part.
func Validate(pw, email string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinRunes {
		return ErrPasswordTooShort
	}
	if n > MaxRunes {
		return ErrPasswordTooLong
	}

	lower := strings.ToLower(pw)
	if strings.EqualFold(pw, email) {
		return ErrPasswordIdentity
	}
	if at := strings.IndexByte(email, '@'); at >= 0 && strings.EqualFold(pw, email[:at]) {
		return ErrPasswordIdentity
	}

	if _, common := commonSet[lower]; common {
		return ErrPasswordCommon
	}
	return nil
}
