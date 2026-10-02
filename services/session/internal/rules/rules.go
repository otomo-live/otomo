// Package rules holds the Level 1 game rules Session enforces: party and friend limits,
// display-name rules and the lobby settings a leader may choose (design/13-player-plane-plan.md
// §2, "Level 1 rules only").
//
// The types mirror the `session.rules` Config namespace field for field, JSON tags
// included (services/config/internal/seed/seed/session.rules/schema.json), so the
// document published in a release decodes straight into Rules. Until the loader (LB-1)
// fetches that document, the service runs on Defaults, which are the seeded values.
//
// Handlers never hold a Rules value across requests. They ask a Source for the current
// one on each call, so a loader that swaps in a newer document changes behaviour on the
// next request without any handler knowing where the rules came from.
package rules

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Rules is one complete, usable set of rules. Build one with Defaults; a value built by
// hand must go through Compile before its name rules can check anything.
type Rules struct {
	Party   Party   `json:"party"`
	Friends Friends `json:"friends"`
	Names   Names   `json:"names"`
	Lobby   Lobby   `json:"lobby"`
}

// Party is the `party` section.
type Party struct {
	MaxSize int `json:"max_size"`
}

// Friends is the `friends` section.
type Friends struct {
	MaxFriends int `json:"max_friends"`
}

// Names is the `names` section: the display-name rules, applied after trimming.
type Names struct {
	MinLength           int      `json:"min_length"`
	MaxLength           int      `json:"max_length"`
	AllowedPattern      string   `json:"allowed_pattern"`
	RenameCooldownHours int      `json:"rename_cooldown_hours"`
	Reserved            []string `json:"reserved"`

	// pattern is AllowedPattern compiled. It is set by Compile and never decoded, so a
	// Rules value cannot carry a pattern that disagrees with its own string.
	pattern *regexp.Regexp
}

// Lobby is the `lobby` section (design/14-launch-handoff.md §2).
type Lobby struct {
	Settings map[string]Setting `json:"settings"`
}

// Setting is one lobby setting: the values a leader may pick and the one a new party
// starts with.
type Setting struct {
	Description string   `json:"description,omitempty"`
	Allowed     []string `json:"allowed"`
	Default     string   `json:"default"`
}

// Defaults returns the compiled-in rules. They equal the values Config seeds into
// `session.rules`, so a fresh stack behaves the same before and after the first release
// that carries the namespace.
func Defaults() *Rules {
	r := &Rules{
		Party:   Party{MaxSize: 4},
		Friends: Friends{MaxFriends: 200},
		Names: Names{
			MinLength:           3,
			MaxLength:           16,
			AllowedPattern:      `^[A-Za-z0-9_]+$`,
			RenameCooldownHours: 24,
			Reserved:            []string{"admin", "moderator", "otomo", "staff", "system"},
		},
		Lobby: Lobby{Settings: map[string]Setting{
			"expedition": {
				Description: "Which expedition the party launches into.",
				Allowed:     []string{"expedition_1"},
				Default:     "expedition_1",
			},
			"difficulty": {
				Description: "Enemy strength for the run.",
				Allowed:     []string{"normal", "hard"},
				Default:     "normal",
			},
		}},
	}
	if err := r.Compile(); err != nil {
		// The defaults are constants in this file. A failure here is a bug caught by the
		// package's own tests, never something a deployment can cause.
		panic(fmt.Sprintf("rules: compiled-in defaults do not compile: %v", err))
	}
	return r
}

// Compile prepares r for use: it compiles the name pattern. It is the one step between
// decoding a document and handing it to handlers.
func (r *Rules) Compile() error {
	re, err := regexp.Compile(r.Names.AllowedPattern)
	if err != nil {
		return fmt.Errorf("names.allowed_pattern: %w", err)
	}
	r.Names.pattern = re
	return nil
}

// RenameCooldown returns the wait between renames as a duration. Zero means a player may
// rename at any time.
func (n Names) RenameCooldown() time.Duration {
	return time.Duration(n.RenameCooldownHours) * time.Hour
}

// NameError is a display name that breaks a rule. Its message is written for the player,
// because it is what the 400 carries.
type NameError struct {
	Reason string
}

func (e *NameError) Error() string { return e.Reason }

// Check applies the name rules to raw and returns the name to store: raw with surrounding
// whitespace removed. Length is counted in Unicode code points, as the schema says, so a
// name is never judged by how many bytes its characters take.
func (n Names) Check(raw string) (string, error) {
	name := strings.TrimSpace(raw)

	if !utf8.ValidString(name) {
		return "", &NameError{Reason: "display_name is not valid UTF-8"}
	}
	if count := utf8.RuneCountInString(name); count < n.MinLength || count > n.MaxLength {
		return "", &NameError{Reason: fmt.Sprintf(
			"display_name must be %d to %d characters long", n.MinLength, n.MaxLength)}
	}
	if n.pattern == nil || !n.pattern.MatchString(name) {
		return "", &NameError{Reason: "display_name contains characters that are not allowed"}
	}
	for _, reserved := range n.Reserved {
		if strings.EqualFold(name, reserved) {
			return "", &NameError{Reason: "that display_name is reserved"}
		}
	}
	return name, nil
}

// Source hands out the rules in force right now. Handlers call Current on every request
// and never keep the result, so whatever sits behind a Source can replace the rules at
// any time.
type Source interface {
	Current() *Rules
}

// Static is a Source that always returns the same rules. It is what the service uses
// until the loader (LB-1) exists, and what tests use to pin a rule.
type Static struct {
	R *Rules
}

// Current returns the fixed rules, or Defaults when none were given.
func (s Static) Current() *Rules {
	if s.R == nil {
		return Defaults()
	}
	return s.R
}
