// Turning a session.rules document into Rules (LB-1).

package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// settingKeyPattern is the schema's propertyNames rule for lobby settings.
var settingKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Parse builds Rules from a session.rules document. Each section the document has
// replaces that section's defaults field by field; a field or section it leaves out
// keeps its compiled-in default (the schema's promise). lobby.settings, when present,
// replaces the default settings as a whole, because a setting the designers removed must
// not survive from the defaults.
//
// A field the schema does not know is an error rather than ignored: the schema forbids
// them, so one here means the document and this code disagree about its shape.
func Parse(doc []byte) (*Rules, error) {
	var sections map[string]json.RawMessage
	if err := strictDecode(doc, &sections); err != nil {
		return nil, fmt.Errorf("document: %w", err)
	}

	r := Defaults()
	for name, raw := range sections {
		var err error
		switch name {
		case "party":
			err = strictDecode(raw, &r.Party)
		case "friends":
			err = strictDecode(raw, &r.Friends)
		case "names":
			err = strictDecode(raw, &r.Names)
		case "lobby":
			var lobby struct {
				Settings map[string]Setting `json:"settings"`
			}
			err = strictDecode(raw, &lobby)
			if err == nil && lobby.Settings != nil {
				r.Lobby.Settings = lobby.Settings
			}
		default:
			err = errors.New("unknown section")
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Validate checks every rule the schema states, and the two cross-field checks it
// cannot express: names.max_length >= names.min_length, and each lobby setting's
// default is one of its allowed values. It also compiles the name pattern, so a Rules
// that passes is ready to use. All problems are reported together.
func (r *Rules) Validate() error {
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}

	check(r.Party.MaxSize >= 1 && r.Party.MaxSize <= 16, "party.max_size %d is not 1 to 16", r.Party.MaxSize)
	check(r.Friends.MaxFriends >= 1 && r.Friends.MaxFriends <= 1000, "friends.max_friends %d is not 1 to 1000", r.Friends.MaxFriends)

	n := r.Names
	check(n.MinLength >= 1 && n.MinLength <= 32, "names.min_length %d is not 1 to 32", n.MinLength)
	check(n.MaxLength >= 1 && n.MaxLength <= 32, "names.max_length %d is not 1 to 32", n.MaxLength)
	check(n.MaxLength >= n.MinLength, "names.max_length %d is below names.min_length %d", n.MaxLength, n.MinLength)
	check(n.RenameCooldownHours >= 0 && n.RenameCooldownHours <= 8760, "names.rename_cooldown_hours %d is not 0 to 8760", n.RenameCooldownHours)
	for _, word := range n.Reserved {
		check(word != "", "names.reserved has an empty entry")
	}
	if err := r.Compile(); err != nil {
		problems = append(problems, err.Error())
	}

	keys := make([]string, 0, len(r.Lobby.Settings))
	for k := range r.Lobby.Settings {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		s := r.Lobby.Settings[key]
		check(settingKeyPattern.MatchString(key), "lobby.settings key %q is not a valid setting name", key)
		check(len(s.Allowed) > 0, "lobby.settings.%s has no allowed values", key)
		check(slices.Contains(s.Allowed, s.Default), "lobby.settings.%s default %q is not one of its allowed values", key, s.Default)
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// strictDecode decodes one JSON value into v, refusing unknown fields and trailing data.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data after the document")
	}
	return nil
}
