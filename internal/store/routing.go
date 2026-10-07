package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const RoutingRetentionDays = 7

// RoutingEvent contains routing metadata only. Conversation is a hashed
// reference, not a client-supplied ID; evidence must never contain request bodies,
// endpoints, tokens, or upstream error messages.
type RoutingEvent struct {
	ID           int64           `json:"id"`
	TS           int64           `json:"ts"`
	Policy       string          `json:"policy"`
	Mode         string          `json:"mode"`
	Kind         string          `json:"kind"`
	Reason       string          `json:"reason"`
	Conversation string          `json:"conversation"`
	SourceID     string          `json:"source_id"`
	TargetID     string          `json:"target_id"`
	Evidence     json.RawMessage `json:"evidence"`
}

func ConversationReference(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

type routingExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// AppendRoutingEvent accepts a DB or the pin-update transaction. A switch and
// its event must commit together; other observations are best-effort.
func AppendRoutingEvent(ctx context.Context, db routingExecutor, e RoutingEvent) error {
	if len(e.Evidence) == 0 {
		e.Evidence = json.RawMessage(`{}`)
	}
	if len(e.Evidence) > 2048 || !json.Valid(e.Evidence) {
		return fmt.Errorf("invalid routing evidence")
	}
	for _, value := range []string{e.Policy, e.Mode, e.Kind, e.Reason, e.Conversation, e.SourceID, e.TargetID} {
		if len(value) > 128 {
			return fmt.Errorf("routing metadata too long")
		}
	}
	_, err := db.ExecContext(ctx, `INSERT INTO routing_event
		(ts,policy,mode,kind,reason,conversation,source_id,target_id,evidence)
		VALUES (?,?,?,?,?,?,?,?,?)`, e.TS, e.Policy, e.Mode, e.Kind, e.Reason,
		e.Conversation, e.SourceID, e.TargetID, string(e.Evidence))
	return err
}

// ListRoutingEvents uses a stable ID cursor. Retention applies to reads even
// when the background janitor has not yet removed old rows.
func ListRoutingEvents(ctx context.Context, db *DB, before int64, limit int, now time.Time) ([]RoutingEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `SELECT id,ts,policy,mode,kind,reason,conversation,source_id,target_id,evidence
		FROM routing_event WHERE ts>=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`,
		now.Add(-RoutingRetentionDays*24*time.Hour).Unix(), before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoutingEvent{}
	for rows.Next() {
		var e RoutingEvent
		var evidence string
		if err := rows.Scan(&e.ID, &e.TS, &e.Policy, &e.Mode, &e.Kind, &e.Reason,
			&e.Conversation, &e.SourceID, &e.TargetID, &evidence); err != nil {
			return nil, err
		}
		e.Evidence = json.RawMessage(evidence)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeRoutingEvents removes at most 1000 rows per call. The observer calls it
// periodically, yielding the single SQLite writer between bounded batches.
func PurgeRoutingEvents(ctx context.Context, db *DB, now time.Time) error {
	_, err := db.ExecContext(ctx, `DELETE FROM routing_event WHERE id IN (
		SELECT id FROM routing_event WHERE ts<? OR id<=COALESCE(
			(SELECT id FROM routing_event ORDER BY id DESC LIMIT 1 OFFSET 50000),0)
		ORDER BY id LIMIT 1000)`, now.Add(-RoutingRetentionDays*24*time.Hour).Unix())
	return err
}
