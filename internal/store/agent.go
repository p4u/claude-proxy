package store

import (
	"context"
	"database/sql"
	"time"
)

type AgentSession struct {
	ConversationKey string
	SessionUUID     string
	CreatedAt       time.Time
	LastUsedAt      time.Time
	NumTurns        int
	TotalCostUSD    float64
}

func UpsertAgentSession(ctx context.Context, db *DB, key, uuid string) error {
	now := time.Now().Unix()
	_, err := db.ExecContext(ctx, `
		INSERT INTO agent_sessions (conversation_key, session_uuid, created_at, last_used_at, num_turns, total_cost_usd)
		VALUES (?, ?, ?, ?, 0, 0)
		ON CONFLICT(conversation_key) DO UPDATE SET last_used_at=?`,
		key, uuid, now, now, now)
	return err
}

func GetAgentSession(ctx context.Context, db *DB, key string) (*AgentSession, error) {
	row := db.QueryRowContext(ctx, `
		SELECT conversation_key, session_uuid, created_at, last_used_at, num_turns, total_cost_usd
		FROM agent_sessions WHERE conversation_key=?`, key)
	s := &AgentSession{}
	var created, last int64
	err := row.Scan(&s.ConversationKey, &s.SessionUUID, &created, &last, &s.NumTurns, &s.TotalCostUSD)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = time.Unix(created, 0)
	s.LastUsedAt = time.Unix(last, 0)
	return s, nil
}

func BumpAgentSession(ctx context.Context, db *DB, key string, cost float64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE agent_sessions
		SET last_used_at=?, num_turns=num_turns+1, total_cost_usd=total_cost_usd+?
		WHERE conversation_key=?`,
		time.Now().Unix(), cost, key)
	return err
}

func ListAgentSessions(ctx context.Context, db *DB) ([]*AgentSession, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT conversation_key, session_uuid, created_at, last_used_at, num_turns, total_cost_usd
		FROM agent_sessions ORDER BY last_used_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentSession
	for rows.Next() {
		s := &AgentSession{}
		var created, last int64
		if err := rows.Scan(&s.ConversationKey, &s.SessionUUID, &created, &last, &s.NumTurns, &s.TotalCostUSD); err != nil {
			return nil, err
		}
		s.CreatedAt = time.Unix(created, 0)
		s.LastUsedAt = time.Unix(last, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

func DeleteAgentSession(ctx context.Context, db *DB, key string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM agent_sessions WHERE conversation_key=?`, key)
	return err
}
