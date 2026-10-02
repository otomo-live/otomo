// Parties (SE-6; design/04-session-minimal.md SES-D1 to SES-D5).
//
// Every change to a party runs in one transaction that locks the party row first
// (SELECT ... FOR UPDATE), then checks and changes membership, then bumps revision. Taking
// the party lock before any member row is the one lock order, so two changes cannot
// deadlock. party_member's primary key is the rule "one party per player": joining a second
// party is a unique violation, never a check a handler has to remember.
//
// A change returns the notices it caused instead of publishing them. The caller publishes
// only after the transaction has committed, so a change that rolls back notifies nobody.
//
// The party is also the lobby (design/14-launch-handoff.md §2). LB-2 adds its state, settings
// and ready flags to Party and Member and a state check to mutate; nothing here assumes a
// party has no state.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"uuid"

	"github.com/otomo-live/otomo/services/session/internal/events"
)

// InviteTTL is how long an invite can be accepted (SES-D2).
const InviteTTL = 5 * time.Minute

// Errors a party change can end in. Each maps to one HTTP answer.
var (
	ErrNotInParty       = errors.New("store: player is not in a party")
	ErrAlreadyInParty   = errors.New("store: player is already in a party")
	ErrNotLeader        = errors.New("store: only the leader may do that")
	ErrPartyFull        = errors.New("store: the party is full")
	ErrInviteNotFound   = errors.New("store: no such invite for this player")
	ErrInviteExpired    = errors.New("store: the invite has expired")
	ErrNotAMember       = errors.New("store: the target is not in this party")
	ErrAlreadyMember    = errors.New("store: the target is already in this party")
	ErrPlayerNotFound   = errors.New("store: no such player")
	ErrBlocked          = errors.New("store: one player has blocked the other")
	ErrTargetIsYourself = errors.New("store: a player cannot target themself")
	// ErrPartyLocked: the lobby is launching or in a match, and only leaving is allowed
	// (design/14-launch-handoff.md §2).
	ErrPartyLocked = errors.New("store: the party is launching or in a match")
	ErrNotReady    = errors.New("store: not every member is ready")
	ErrNotInGame   = errors.New("store: the party is not in a match")
)

// The lobby states (design/14-launch-handoff.md §2).
const (
	StateForming   = "forming"
	StateLaunching = "launching"
	StateInGame    = "in_game"
)

// RevisionMismatchError is a leader call made against a revision that is no longer the
// party's. Current is the party's revision now, so the client can refetch.
type RevisionMismatchError struct {
	Current int
}

func (e *RevisionMismatchError) Error() string {
	return fmt.Sprintf("store: stale revision; the party is at %d", e.Current)
}

// Party is a party and its members, as GET /party shows it.
type Party struct {
	ID        string
	LeaderID  string
	MaxSize   int
	Revision  int
	CreatedAt time.Time
	Members   []Member // longest-standing first

	// The lobby (LB-2): its state, the leader's settings (one string per session.rules
	// lobby setting), when the state last changed, and the match while in_game.
	State          string
	Settings       map[string]string
	StateChangedAt time.Time
	AllocationID   string // "" unless in_game
	MatchAddress   string // the Gameplay Proxy's public address while in_game (LB-3)
	MatchPort      int
}

// Member is one party member with their public name.
type Member struct {
	PlayerID      string
	DisplayName   string // empty if the member has no profile
	Discriminator int
	JoinedAt      time.Time
	Ready         bool
}

// requireForming refuses a lobby change outside forming.
func requireForming(p *Party) error {
	if p.State != StateForming {
		return ErrPartyLocked
	}
	return nil
}

// Invite is one party invite.
type Invite struct {
	ID         string
	PartyID    string
	FromPlayer string
	ToPlayer   string
	ExpiresAt  time.Time
}

// Notice is one event a committed change owes a player. The caller publishes it.
type Notice struct {
	PlayerID string
	Type     string
	Payload  map[string]any
}

// memberIDs returns the ids of p's members.
func (p *Party) memberIDs() []string {
	ids := make([]string, len(p.Members))
	for i, m := range p.Members {
		ids[i] = m.PlayerID
	}
	return ids
}

func (p *Party) has(player string) bool {
	for _, m := range p.Members {
		if m.PlayerID == player {
			return true
		}
	}
	return false
}

// updated returns a party.updated notice for each member of p. Every party event carries
// party_id and revision (design/14-launch-handoff.md §7); party.updated also carries the
// lobby's state, settings and each member's ready flag (LB-2), so a client can update
// its lobby screen without a refetch. revision still decides which event is newest.
func updated(p *Party) []Notice {
	ready := make(map[string]bool, len(p.Members))
	for _, m := range p.Members {
		ready[m.PlayerID] = m.Ready
	}
	out := make([]Notice, 0, len(p.Members))
	for _, id := range p.memberIDs() {
		out = append(out, Notice{PlayerID: id, Type: events.TypePartyUpdated,
			Payload: map[string]any{
				"party_id": p.ID,
				"revision": p.Revision,
				"state":    p.State,
				"settings": p.Settings,
				"ready":    ready,
			}})
	}
	return out
}

// queryer is what loadParty needs: a pool or a transaction.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadParty reads a party and its members. With lock set, the party row is locked for
// the rest of tx. It returns ErrNotInParty when the party does not exist.
func loadParty(ctx context.Context, q queryer, partyID string, lock bool) (*Party, error) {
	sql := `SELECT party_id::text, leader_id::text, max_size, revision, created_at,
	               state, settings, state_changed_at, coalesce(allocation_id::text, ''),
	               coalesce(match_address, ''), coalesce(match_port, 0)
	          FROM party WHERE party_id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	p := &Party{}
	var maxSize int16
	var settings []byte
	err := q.QueryRow(ctx, sql, partyID).Scan(&p.ID, &p.LeaderID, &maxSize, &p.Revision, &p.CreatedAt,
		&p.State, &settings, &p.StateChangedAt, &p.AllocationID, &p.MatchAddress, &p.MatchPort)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotInParty
	}
	if err != nil {
		return nil, fmt.Errorf("store: read party: %w", err)
	}
	p.MaxSize = int(maxSize)
	if err := json.Unmarshal(settings, &p.Settings); err != nil {
		return nil, fmt.Errorf("store: read party settings: %w", err)
	}
	if p.Settings == nil {
		p.Settings = map[string]string{}
	}

	rows, err := q.Query(ctx,
		`SELECT m.player_id::text, coalesce(pp.display_name, ''), coalesce(pp.discriminator, 0), m.joined_at, m.ready
		   FROM party_member m
		   LEFT JOIN player_profile pp ON pp.player_id = m.player_id
		  WHERE m.party_id = $1
		  ORDER BY m.joined_at, m.player_id`, partyID)
	if err != nil {
		return nil, fmt.Errorf("store: read party members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.PlayerID, &m.DisplayName, &m.Discriminator, &m.JoinedAt, &m.Ready); err != nil {
			return nil, fmt.Errorf("store: read party member: %w", err)
		}
		p.Members = append(p.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read party members: %w", err)
	}
	return p, nil
}

// partyOf returns the id of player's party, or ErrNotInParty.
func partyOf(ctx context.Context, q queryer, player string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT party_id::text FROM party_member WHERE player_id = $1`, player).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotInParty
	}
	if err != nil {
		return "", fmt.Errorf("store: find party of player: %w", err)
	}
	return id, nil
}

// bumpRevision adds one to p's revision, in the database and in p.
func bumpRevision(ctx context.Context, tx pgx.Tx, p *Party) error {
	err := tx.QueryRow(ctx,
		`UPDATE party SET revision = revision + 1 WHERE party_id = $1 RETURNING revision`,
		p.ID).Scan(&p.Revision)
	if err != nil {
		return fmt.Errorf("store: bump party revision: %w", err)
	}
	return nil
}

// mutate runs change against player's party, locked, in one transaction, and returns the
// party as it is after the change together with the notices the change owes.
//
// The membership is looked up without a lock and checked again under it: a player kicked
// between the two reads is not in the party any more, and is told so.
func (d *DB) mutate(ctx context.Context, player string,
	change func(tx pgx.Tx, p *Party) ([]Notice, error)) (*Party, []Notice, error) {

	var (
		party   *Party
		notices []Notice
	)
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		id, err := partyOf(ctx, tx, player)
		if err != nil {
			return err
		}
		p, err := loadParty(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if !p.has(player) {
			return ErrNotInParty
		}
		n, err := change(tx, p)
		if err != nil {
			return err
		}
		if d.beforeCommit != nil {
			if err := d.beforeCommit(ctx, tx); err != nil {
				return err
			}
		}
		party, notices = p, n
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return party, notices, nil
}

// checkLeader refuses a leader call from anyone else, or against a stale revision.
func checkLeader(p *Party, player string, revision int) error {
	if p.LeaderID != player {
		return ErrNotLeader
	}
	if revision != p.Revision {
		return &RevisionMismatchError{Current: p.Revision}
	}
	return nil
}

// reload re-reads p's members after a change inside tx, so the returned party and its
// notices describe the state being committed.
func reload(ctx context.Context, tx pgx.Tx, p *Party) error {
	fresh, err := loadParty(ctx, tx, p.ID, false)
	if err != nil {
		return err
	}
	*p = *fresh
	return nil
}

// CreateParty makes a party with player as its leader and only member (SES-D1), in
// forming with settings (the rules' defaults: design/14-launch-handoff.md §2 starts a new
// lobby with every default).
func (d *DB) CreateParty(ctx context.Context, player string, maxSize int, settings map[string]string) (*Party, []Notice, error) {
	if settings == nil {
		settings = map[string]string{}
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, fmt.Errorf("store: encode party settings: %w", err)
	}
	var party *Party
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		id := uuid.NewV7().String()
		if _, err := tx.Exec(ctx,
			`INSERT INTO party (party_id, leader_id, max_size, settings) VALUES ($1, $2, $3, $4)`,
			id, player, maxSize, body); err != nil {
			return fmt.Errorf("store: create party: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO party_member (player_id, party_id) VALUES ($1, $2)`, player, id); err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyInParty
			}
			return fmt.Errorf("store: add party leader: %w", err)
		}
		p, err := loadParty(ctx, tx, id, false)
		if err != nil {
			return err
		}
		party = p
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return party, updated(party), nil
}

// GetParty returns player's party, or ErrNotInParty.
func (d *DB) GetParty(ctx context.Context, player string) (*Party, error) {
	id, err := partyOf(ctx, d.Pool, player)
	if err != nil {
		return nil, err
	}
	p, err := loadParty(ctx, d.Pool, id, false)
	if errors.Is(err, ErrNotInParty) {
		return nil, ErrNotInParty // disbanded between the two reads
	}
	return p, err
}

// InviteToParty invites target to player's party (SES-D2). Any member may invite.
// Inviting someone who already has an invite to this party refreshes its expiry and
// keeps its id. maxSize is the party size limit in force now.
func (d *DB) InviteToParty(ctx context.Context, player, target string, maxSize int) (*Invite, []Notice, error) {
	if player == target {
		return nil, nil, ErrTargetIsYourself
	}
	var inv *Invite
	_, notices, err := d.mutate(ctx, player, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		var exists, blocked bool
		err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM player_profile WHERE player_id = $1),
			        EXISTS (SELECT 1 FROM block
			                 WHERE (blocker = $1 AND blocked = $2) OR (blocker = $2 AND blocked = $1))`,
			target, player).Scan(&exists, &blocked)
		if err != nil {
			return nil, fmt.Errorf("store: check invite target: %w", err)
		}
		switch {
		case p.State != StateForming:
			return nil, ErrPartyLocked
		case !exists:
			return nil, ErrPlayerNotFound
		case blocked:
			return nil, ErrBlocked
		case p.has(target):
			return nil, ErrAlreadyMember
		case len(p.Members) >= maxSize:
			return nil, ErrPartyFull
		}

		inv = &Invite{PartyID: p.ID, FromPlayer: player, ToPlayer: target}
		err = tx.QueryRow(ctx,
			`INSERT INTO party_invite (invite_id, party_id, from_player, to_player, expires_at)
			 VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
			 ON CONFLICT (party_id, to_player)
			 DO UPDATE SET from_player = EXCLUDED.from_player, expires_at = EXCLUDED.expires_at
			 RETURNING invite_id::text, expires_at`,
			uuid.NewV7().String(), p.ID, player, target, InviteTTL.Seconds()).Scan(&inv.ID, &inv.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("store: write invite: %w", err)
		}
		return []Notice{{PlayerID: target, Type: events.TypePartyInvite, Payload: map[string]any{
			"invite_id":   inv.ID,
			"party_id":    p.ID,
			"revision":    p.Revision,
			"from_player": player,
			"expires_at":  inv.ExpiresAt,
		}}}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return inv, notices, nil
}

// AcceptInvite puts player into the party that invited them (SES-D3). The party row is
// locked before the size check, so concurrent accepts can never overfill it; maxSize is
// the limit in force now. Joining while already in a party is ErrAlreadyInParty.
func (d *DB) AcceptInvite(ctx context.Context, player, inviteID string, maxSize int) (*Party, []Notice, error) {
	var (
		party   *Party
		notices []Notice
	)
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var partyID string
		var expired, blocked bool
		err := tx.QueryRow(ctx,
			`SELECT i.party_id::text, i.expires_at <= now(),
			        EXISTS (SELECT 1 FROM block
			                 WHERE (blocker = i.from_player AND blocked = i.to_player)
			                    OR (blocker = i.to_player AND blocked = i.from_player))
			   FROM party_invite i
			  WHERE i.invite_id = $1 AND i.to_player = $2`, inviteID, player).Scan(&partyID, &expired, &blocked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInviteNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read invite: %w", err)
		}
		if expired {
			return ErrInviteExpired
		}
		// A block normally deletes the invite with it (SE-5); this catches an invite
		// written by a transaction that raced the block.
		if blocked {
			return ErrInviteNotFound
		}

		p, err := loadParty(ctx, tx, partyID, true)
		if errors.Is(err, ErrNotInParty) {
			return ErrInviteNotFound // the party was disbanded
		}
		if err != nil {
			return err
		}
		if err := requireForming(p); err != nil {
			return err
		}
		if len(p.Members) >= maxSize {
			return ErrPartyFull
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO party_member (player_id, party_id) VALUES ($1, $2)`, player, p.ID); err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyInParty
			}
			return fmt.Errorf("store: join party: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM party_invite WHERE invite_id = $1`, inviteID); err != nil {
			return fmt.Errorf("store: remove accepted invite: %w", err)
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return err
		}
		if err := reload(ctx, tx, p); err != nil {
			return err
		}
		if d.beforeCommit != nil {
			if err := d.beforeCommit(ctx, tx); err != nil {
				return err
			}
		}
		party, notices = p, updated(p)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return party, notices, nil
}

// DeclineInvite removes an invite addressed to player.
func (d *DB) DeclineInvite(ctx context.Context, player, inviteID string) error {
	tag, err := d.Pool.Exec(ctx,
		`DELETE FROM party_invite WHERE invite_id = $1 AND to_player = $2`, inviteID, player)
	if err != nil {
		return fmt.Errorf("store: decline invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInviteNotFound
	}
	return nil
}

// LeaveParty removes player from their party (SES-D4). A leader who leaves hands the
// party to the longest-standing member (earliest joined_at, then player_id). The last
// member leaving disbands the party. The returned party is nil when it was disbanded.
func (d *DB) LeaveParty(ctx context.Context, player string) (*Party, []Notice, error) {
	disbanded := false
	p, notices, err := d.mutate(ctx, player, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		if _, err := tx.Exec(ctx, `DELETE FROM party_member WHERE player_id = $1`, player); err != nil {
			return nil, fmt.Errorf("store: leave party: %w", err)
		}
		if len(p.Members) == 1 {
			if _, err := tx.Exec(ctx, `DELETE FROM party WHERE party_id = $1`, p.ID); err != nil {
				return nil, fmt.Errorf("store: disband empty party: %w", err)
			}
			disbanded = true
			return nil, nil
		}
		if p.LeaderID == player {
			// Members are ordered longest-standing first, and the leaver is still in the
			// list read under the lock, so the heir is the first member who is not them.
			heir := p.Members[0].PlayerID
			if heir == player {
				heir = p.Members[1].PlayerID
			}
			if _, err := tx.Exec(ctx,
				`UPDATE party SET leader_id = $2 WHERE party_id = $1`, p.ID, heir); err != nil {
				return nil, fmt.Errorf("store: promote heir: %w", err)
			}
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return nil, err
		}
		if err := reload(ctx, tx, p); err != nil {
			return nil, err
		}
		return updated(p), nil
	})
	if err != nil {
		return nil, nil, err
	}
	if disbanded {
		return nil, nil, nil
	}
	return p, notices, nil
}

// KickFromParty removes target from the leader's party. The kicked player is told with
// party.kicked, everyone left with party.updated.
func (d *DB) KickFromParty(ctx context.Context, leader, target string, revision int) (*Party, []Notice, error) {
	if leader == target {
		return nil, nil, ErrTargetIsYourself
	}
	return d.mutate(ctx, leader, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		if err := checkLeader(p, leader, revision); err != nil {
			return nil, err
		}
		if err := requireForming(p); err != nil {
			return nil, err
		}
		if !p.has(target) {
			return nil, ErrNotAMember
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM party_member WHERE player_id = $1 AND party_id = $2`, target, p.ID); err != nil {
			return nil, fmt.Errorf("store: kick: %w", err)
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return nil, err
		}
		if err := reload(ctx, tx, p); err != nil {
			return nil, err
		}
		kicked := Notice{PlayerID: target, Type: events.TypePartyKicked,
			Payload: map[string]any{"party_id": p.ID, "revision": p.Revision}}
		return append(updated(p), kicked), nil
	})
}

// PromoteInParty makes target the leader of the leader's party.
func (d *DB) PromoteInParty(ctx context.Context, leader, target string, revision int) (*Party, []Notice, error) {
	if leader == target {
		return nil, nil, ErrTargetIsYourself
	}
	return d.mutate(ctx, leader, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		if err := checkLeader(p, leader, revision); err != nil {
			return nil, err
		}
		if err := requireForming(p); err != nil {
			return nil, err
		}
		if !p.has(target) {
			return nil, ErrNotAMember
		}
		if _, err := tx.Exec(ctx,
			`UPDATE party SET leader_id = $2 WHERE party_id = $1`, p.ID, target); err != nil {
			return nil, fmt.Errorf("store: promote: %w", err)
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return nil, err
		}
		if err := reload(ctx, tx, p); err != nil {
			return nil, err
		}
		return updated(p), nil
	})
}

// UpdatePartySettings merges changes into the leader's lobby settings (LB-2). A key the
// request leaves out keeps its value. The caller has already checked every key and
// value against the rules. Changing the settings clears every member's ready flag, since
// they readied up for the old ones; a request that changes nothing is a no-op that
// neither clears the flags nor bumps the revision.
func (d *DB) UpdatePartySettings(ctx context.Context, leader string, changes map[string]string, revision int) (*Party, []Notice, error) {
	return d.mutate(ctx, leader, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		if err := checkLeader(p, leader, revision); err != nil {
			return nil, err
		}
		if err := requireForming(p); err != nil {
			return nil, err
		}
		merged := make(map[string]string, len(p.Settings)+len(changes))
		for k, v := range p.Settings {
			merged[k] = v
		}
		changed := false
		for k, v := range changes {
			if merged[k] != v {
				changed = true
			}
			merged[k] = v
		}
		if !changed {
			return nil, nil
		}
		body, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("store: encode party settings: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE party SET settings = $2 WHERE party_id = $1`, p.ID, body); err != nil {
			return nil, fmt.Errorf("store: write party settings: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE party_member SET ready = false WHERE party_id = $1`, p.ID); err != nil {
			return nil, fmt.Errorf("store: clear ready flags: %w", err)
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return nil, err
		}
		if err := reload(ctx, tx, p); err != nil {
			return nil, err
		}
		return updated(p), nil
	})
}

// SetReady sets player's ready flag in their lobby (LB-2). Any member may; only in
// forming. Setting the flag it already has is a no-op.
func (d *DB) SetReady(ctx context.Context, player string, ready bool) (*Party, []Notice, error) {
	return d.mutate(ctx, player, func(tx pgx.Tx, p *Party) ([]Notice, error) {
		if err := requireForming(p); err != nil {
			return nil, err
		}
		for _, m := range p.Members {
			if m.PlayerID == player && m.Ready == ready {
				return nil, nil
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE party_member SET ready = $2 WHERE player_id = $1`, player, ready); err != nil {
			return nil, fmt.Errorf("store: set ready: %w", err)
		}
		if err := bumpRevision(ctx, tx, p); err != nil {
			return nil, err
		}
		if err := reload(ctx, tx, p); err != nil {
			return nil, err
		}
		return updated(p), nil
	})
}

// DeleteExpiredInvites removes invites past their expiry and returns how many (SES-D5).
func (d *DB) DeleteExpiredInvites(ctx context.Context) (int64, error) {
	tag, err := d.Pool.Exec(ctx, `DELETE FROM party_invite WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired invites: %w", err)
	}
	return tag.RowsAffected(), nil
}
