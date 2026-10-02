package store

import (
	"errors"
	"testing"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

var seedSettings = map[string]string{"expedition": "expedition_1", "difficulty": "normal"}

// lock puts party into state directly, as LB-3's launch would.
func lock(t *testing.T, db *DB, partyID, state string) {
	t.Helper()
	if _, err := db.Pool.Exec(t.Context(),
		`UPDATE party SET state = $2, state_changed_at = now() WHERE party_id = $1`, partyID, state); err != nil {
		t.Fatal(err)
	}
}

func TestANewLobbyIsFormingWithItsSettings(t *testing.T) {
	db := testDB(t)
	a := player(t, db)
	p, notices, err := db.CreateParty(t.Context(), a, 4, seedSettings)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateForming || p.Settings["difficulty"] != "normal" || len(p.Settings) != 2 || p.Members[0].Ready {
		t.Errorf("new party = %+v", p)
	}
	payload := notices[0].Payload
	if payload["state"] != StateForming || payload["settings"] == nil || payload["ready"] == nil {
		t.Errorf("party.updated payload = %v, want state, settings and ready", payload)
	}
}

// TestChangingSettingsClearsEveryReady is doc 14 §2.
func TestChangingSettingsClearsEveryReady(t *testing.T) {
	db := testDB(t)
	leader, m := player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), leader, 4, seedSettings); err != nil {
		t.Fatal(err)
	}
	join(t, db, leader, m)
	if _, _, err := db.SetReady(t.Context(), leader, true); err != nil {
		t.Fatal(err)
	}
	p, notices, err := db.SetReady(t.Context(), m, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Members[0].Ready || !p.Members[1].Ready {
		t.Fatalf("after both readied = %+v", p.Members)
	}
	if ready, _ := notices[0].Payload["ready"].(map[string]bool); !ready[m] {
		t.Errorf("party.updated ready = %v, want %s ready", notices[0].Payload["ready"], m)
	}

	changed, notices, err := db.UpdatePartySettings(t.Context(), leader, map[string]string{"difficulty": "hard"}, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Settings["difficulty"] != "hard" || changed.Settings["expedition"] != "expedition_1" {
		t.Errorf("settings = %v, want difficulty changed and expedition kept", changed.Settings)
	}
	for _, mem := range changed.Members {
		if mem.Ready {
			t.Errorf("%s is still ready after the settings changed", mem.PlayerID)
		}
	}
	if changed.Revision != p.Revision+1 || len(notices) != 2 || notices[0].Type != events.TypePartyUpdated {
		t.Errorf("revision %d notices %+v", changed.Revision, notices)
	}

	// Sending the same settings again changes nothing: no revision bump, no events.
	same, notices, err := db.UpdatePartySettings(t.Context(), leader, map[string]string{"difficulty": "hard"}, changed.Revision)
	if err != nil || same.Revision != changed.Revision || len(notices) != 0 {
		t.Errorf("no-op settings = revision %d, %d notices, %v", same.Revision, len(notices), err)
	}
}

func TestSettingsAreALeaderCall(t *testing.T) {
	db := testDB(t)
	leader, m := player(t, db), player(t, db)
	if _, _, err := db.CreateParty(t.Context(), leader, 4, seedSettings); err != nil {
		t.Fatal(err)
	}
	p := join(t, db, leader, m)

	if _, _, err := db.UpdatePartySettings(t.Context(), m, map[string]string{"difficulty": "hard"}, p.Revision); !errors.Is(err, ErrNotLeader) {
		t.Errorf("settings by a member = %v, want ErrNotLeader", err)
	}
	var stale *RevisionMismatchError
	if _, _, err := db.UpdatePartySettings(t.Context(), leader, map[string]string{"difficulty": "hard"}, p.Revision-1); !errors.As(err, &stale) {
		t.Errorf("settings at a stale revision = %v, want RevisionMismatchError", err)
	}
}

// TestALockedLobbyRefusesEveryChangeButLeaving is doc 14 §2's table for launching and
// in_game: 409 party_locked for invites, accepts, settings, ready, kick and promote;
// leaving is always allowed.
func TestALockedLobbyRefusesEveryChangeButLeaving(t *testing.T) {
	for _, state := range []string{StateLaunching, StateInGame} {
		t.Run(state, func(t *testing.T) {
			db := testDB(t)
			leader, m, outsider, invitee := player(t, db), player(t, db), player(t, db), player(t, db)
			created, _, err := db.CreateParty(t.Context(), leader, 4, seedSettings)
			if err != nil {
				t.Fatal(err)
			}
			join(t, db, leader, m)
			pending, _, err := db.InviteToParty(t.Context(), leader, invitee, 4)
			if err != nil {
				t.Fatal(err)
			}
			lock(t, db, created.ID, state)
			p, err := db.GetParty(t.Context(), leader)
			if err != nil || p.State != state {
				t.Fatalf("GetParty = %+v, %v", p, err)
			}

			checks := map[string]error{}
			_, _, checks["invite"] = db.InviteToParty(t.Context(), leader, outsider, 4)
			_, _, checks["accept"] = db.AcceptInvite(t.Context(), invitee, pending.ID, 4)
			_, _, checks["settings"] = db.UpdatePartySettings(t.Context(), leader, map[string]string{"difficulty": "hard"}, p.Revision)
			_, _, checks["ready"] = db.SetReady(t.Context(), m, true)
			_, _, checks["kick"] = db.KickFromParty(t.Context(), leader, m, p.Revision)
			_, _, checks["promote"] = db.PromoteInParty(t.Context(), leader, m, p.Revision)
			for name, err := range checks {
				if !errors.Is(err, ErrPartyLocked) {
					t.Errorf("%s while %s = %v, want ErrPartyLocked", name, state, err)
				}
			}

			// Leaving is always allowed, and a leader who leaves still hands over.
			after, _, err := db.LeaveParty(t.Context(), leader)
			if err != nil || after.LeaderID != m || after.State != state {
				t.Errorf("leave while %s = %+v, %v; want m to lead and the state kept", state, after, err)
			}
		})
	}
}
