package usertoken

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

var ErrNotFound = errors.New("user token not found")

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// UserToken is a named bearer token used to authenticate a specific user.
type UserToken struct {
	ID         string
	Name       string
	Token      string
	Status     Status
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// contextKey is unexported to prevent collisions across packages.
type contextKey int

const identityKey contextKey = iota

// Identity is injected into the request context by the auth middleware.
type Identity struct {
	IsAdmin     bool   // matched the master PROXY_AUTH_TOKEN
	UserTokenID string // non-empty when a user token was matched
	UserName    string
}

// FromContext returns the identity attached by the auth middleware, or nil.
func FromContext(ctx context.Context) *Identity {
	v, _ := ctx.Value(identityKey).(*Identity)
	return v
}

// WithIdentity returns a copy of ctx with the given identity attached.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "utok_" + hex.EncodeToString(b[:])
}

// GenerateToken produces a cryptographically random 64-character hex token.
func GenerateToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Create inserts a new user token and returns it.
func Create(ctx context.Context, db *store.DB, name string) (*UserToken, error) {
	ut := &UserToken{
		ID:        newID(),
		Name:      name,
		Token:     GenerateToken(),
		Status:    StatusActive,
		CreatedAt: time.Now(),
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO user_tokens (id, name, token, status, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		ut.ID, ut.Name, ut.Token, string(ut.Status), ut.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return ut, nil
}

// List returns all user tokens ordered by creation time.
func List(ctx context.Context, db *store.DB) ([]*UserToken, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, token, status, created_at, last_used_at
		FROM user_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserToken
	for rows.Next() {
		ut, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ut)
	}
	return out, rows.Err()
}

// Get returns a single user token by ID.
func Get(ctx context.Context, db *store.DB, id string) (*UserToken, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, token, status, created_at, last_used_at
		FROM user_tokens WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scan(rows)
}

// LookupByToken finds a user token by its bearer value (used in hot path).
func LookupByToken(ctx context.Context, db *store.DB, token string) (*UserToken, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, token, status, created_at, last_used_at
		FROM user_tokens WHERE token=?`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scan(rows)
}

// SetStatus sets a token's status to active or disabled.
func SetStatus(ctx context.Context, db *store.DB, id string, s Status) error {
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET status=? WHERE id=?`, string(s), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a user token and its associated request_log rows.
func Delete(ctx context.Context, db *store.DB, id string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM user_tokens WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Refresh generates a new token value for the user, returns the new token.
func Refresh(ctx context.Context, db *store.DB, id string) (string, error) {
	token := GenerateToken()
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET token=? WHERE id=?`, token, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// MarkUsed updates last_used_at for a token (called on authenticated request).
func MarkUsed(ctx context.Context, db *store.DB, id string) {
	_, _ = db.ExecContext(ctx,
		`UPDATE user_tokens SET last_used_at=? WHERE id=?`, time.Now().Unix(), id)
}

// HasAny reports whether at least one user token exists in the database.
func HasAny(ctx context.Context, db *store.DB) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_tokens`).Scan(&n)
	return n > 0
}

func scan(rs interface{ Scan(...any) error }) (*UserToken, error) {
	ut := &UserToken{}
	var created int64
	var lu sql.NullInt64
	var status string
	if err := rs.Scan(&ut.ID, &ut.Name, &ut.Token, &status, &created, &lu); err != nil {
		return nil, err
	}
	ut.Status = Status(status)
	ut.CreatedAt = time.Unix(created, 0)
	if lu.Valid {
		t := time.Unix(lu.Int64, 0)
		ut.LastUsedAt = &t
	}
	return ut, nil
}
