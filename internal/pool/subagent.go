package pool

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
)

const (
	// familyHeadroom is the least headroom, Room(7d)^SevenDayExp,
	// a candidate needs for a subagent's first binding to avoid its family's
	// credentials. Below it on every other candidate, the ordinary pick runs.
	familyHeadroom = 0.25
	// familyLive is how recently a sibling subagent must have been seen for
	// its credential to count as in use by the family.
	familyLive = 15 * time.Minute
)

// familyQuery lists the session's own binding at any age and its children seen
// since the cut-off, a primary-key range [family+"/", family+"0") ('0' follows
// '/'). Bind the bounds as Go strings: a []byte bind compares as BLOB.
const familyQuery = `
	SELECT credential_id FROM conversations WHERE id = ?
	UNION
	SELECT credential_id FROM conversations
	WHERE id >= ? AND id < ? AND last_seen_at >= ?`

// subagentKey resolves a valid candidate to its child key, its family and
// startChild, with no existence check: bindOnce picks a new row's credential.
// Options that are not a child of convID on an unscoped Anthropic binding keep convID.
func subagentKey(convID string, prov provider.ID, scope string, allowed []string, opts RequestOptions) (key, family string, startChild bool) {
	if opts.Child == "" || opts.Family == "" || opts.Family != convID || !strings.HasPrefix(opts.Child, opts.Family+"/") ||
		(prov != "" && prov != provider.Anthropic) || scope != "" || len(allowed) > 0 {
		return convID, "", false
	}
	return opts.Child, opts.Family, opts.StartChild
}

// pickNewChildLocked picks the credential of a new child binding. A request
// that may start the child avoids the family; any other (a fork, its side
// requests) stays on the session's credential so its prompt cache survives.
func (p *Pool) pickNewChildLocked(ctx context.Context, tx *sql.Tx, prov provider.ID, allowed []string, family string, startChild bool) (string, error) {
	if startChild {
		return p.pickChildLocked(ctx, tx, prov, allowed, family)
	}
	id, err := familyPin(ctx, tx, prov, family)
	if err != nil || id != "" {
		return id, err
	}
	return p.pickActiveLocked(ctx, tx, prov, allowed)
}

// familyPin returns the credential of family's own binding while it can
// still serve (active or limited; a limited child pin moves on its next
// request), or "" when there is none.
func familyPin(ctx context.Context, q rebalanceReader, prov provider.ID, family string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT c.id FROM conversations v JOIN credentials c ON c.id = v.credential_id
		WHERE v.id = ? AND c.status IN ('active','limited') AND COALESCE(c.provider,'anthropic') = ?`,
		family, string(prov)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// pickChildLocked picks a new child binding away from the family's credentials,
// among candidates with familyHeadroom left; with none, the ordinary pick runs.
func (p *Pool) pickChildLocked(ctx context.Context, tx *sql.Tx, prov provider.ID, allowed []string, family string) (string, error) {
	now := time.Now()
	exclude, err := familyCredentials(ctx, tx, family, now)
	if err != nil {
		return "", err
	}
	if len(exclude) > 0 {
		candidates, err := activeCandidates(ctx, tx, prov, allowed, exclude, now)
		if err != nil {
			return "", err
		}
		if candidates, err = withHeadroom(ctx, tx, candidates, familyHeadroom); err != nil {
			return "", err
		}
		if len(candidates) > 0 {
			return p.weightedRandPick(ctx, tx, candidates)
		}
	}
	return p.pickActiveLocked(ctx, tx, prov, allowed)
}

// familyCredentials returns the credentials of family's own binding and of
// its children seen within familyLive of now.
func familyCredentials(ctx context.Context, q rebalanceReader, family string, now time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, familyQuery, family, family+"/", family+"0", now.Add(-familyLive).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// withHeadroom keeps the candidates whose latest usage snapshot leaves at
// least floor of headroom (Score at weight 1). A credential without a
// snapshot has full headroom, as in weightedRandPick.
func withHeadroom(ctx context.Context, q rebalanceReader, candidates []weightedEntry, floor float64) ([]weightedEntry, error) {
	var kept []weightedEntry
	for _, e := range candidates {
		var sd float64
		err := q.QueryRowContext(ctx, `
			SELECT seven_day_pct FROM usage_history
			WHERE credential_id=? ORDER BY captured_at DESC LIMIT 1`, e.id).Scan(&sd)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if Score(1, sd) >= floor {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// limitedNow reports a credential cooling down after a 429.
func limitedNow(c *creds.Credential, now time.Time) bool {
	return c.Status == creds.StatusLimited && c.RetryAfter != nil && c.RetryAfter.After(now)
}

// pickActiveOnly is the ordinary pick without the limited fallback:
// ErrNoCredentials when no active candidate is left.
func (p *Pool) pickActiveOnly(ctx context.Context, tx rebalanceReader, prov provider.ID, allowed []string) (string, error) {
	candidates, err := activeCandidates(ctx, tx, prov, allowed, nil, time.Now())
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", ErrNoCredentials
	}
	return p.weightedRandPick(ctx, tx, candidates)
}
