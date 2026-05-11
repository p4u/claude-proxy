// Package users manages per-user authentication tokens for the proxy.
package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

// User represents a proxy user.
type User struct {
	ID          int64
	Name        string
	CreatedAt   time.Time
	LastUsedAt  time.Time // zero if never used
	TokenSHA256 string    // hex SHA-256 of raw token; populated by List and Create
}

// Store manages users in SQLite.
type Store struct {
	db *store.DB
}

// NewStore creates a Store backed by db.
func NewStore(db *store.DB) *Store {
	return &Store{db: db}
}

// Create inserts a new user with a freshly generated token.
// Returns the user and the raw token (shown once; never stored).
func (s *Store) Create(ctx context.Context, name string) (User, string, error) {
	if name == "" {
		return User{}, "", fmt.Errorf("name must not be empty")
	}

	rawToken, hash, err := generateToken()
	if err != nil {
		return User{}, "", fmt.Errorf("generate token: %w", err)
	}

	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (name, token_sha256, created_at) VALUES (?, ?, ?)`,
		name, hash, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, "", fmt.Errorf("user %q already exists", name)
		}
		return User{}, "", fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, "", fmt.Errorf("get id: %w", err)
	}
	u := User{
		ID:          id,
		Name:        name,
		CreatedAt:   time.Unix(now, 0),
		TokenSHA256: hash,
	}
	return u, rawToken, nil
}

// Lookup finds a user by raw token. Returns an error if not found or on db failure.
func (s *Store) Lookup(ctx context.Context, rawToken string) (User, error) {
	hash := hashToken(rawToken)
	var u User
	var created int64
	var lastUsed sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at, last_used_at FROM users WHERE token_sha256=?`, hash,
	).Scan(&u.ID, &u.Name, &created, &lastUsed)
	if err == sql.ErrNoRows {
		return User{}, fmt.Errorf("token not found")
	}
	if err != nil {
		return User{}, fmt.Errorf("lookup: %w", err)
	}
	u.CreatedAt = time.Unix(created, 0)
	if lastUsed.Valid {
		u.LastUsedAt = time.Unix(lastUsed.Int64, 0)
	}
	return u, nil
}

// List returns all users ordered by id ascending.
func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, token_sha256, created_at, last_used_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Name, &u.TokenSHA256, &created, &lastUsed); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(created, 0)
		if lastUsed.Valid {
			u.LastUsedAt = time.Unix(lastUsed.Int64, 0)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Revoke deletes a user by name or numeric ID string.
// Returns an error if the user does not exist.
func (s *Store) Revoke(ctx context.Context, nameOrID string) error {
	var res sql.Result
	var err error
	if id, perr := strconv.ParseInt(nameOrID, 10, 64); perr == nil {
		res, err = s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM users WHERE name=?`, nameOrID)
	}
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", nameOrID)
	}
	return nil
}

// TouchLastUsed updates last_used_at for the given user id (best-effort).
func (s *Store) TouchLastUsed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_used_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

// TokenFingerprint returns the first 8 hex characters of a SHA-256 hash string.
func TokenFingerprint(sha256hex string) string {
	if len(sha256hex) >= 8 {
		return sha256hex[:8]
	}
	return sha256hex
}

// generateToken creates a raw token (cp_<base64url>) and its hex SHA-256 hash.
func generateToken() (rawToken, hash string, err error) {
	b := make([]byte, 32)
	if _, rerr := rand.Read(b); rerr != nil {
		return "", "", rerr
	}
	rawToken = "cp_" + base64.RawURLEncoding.EncodeToString(b)
	hash = hashToken(rawToken)
	return rawToken, hash, nil
}

// hashToken returns the hex-encoded SHA-256 of the token string.
func hashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strContains(s, "UNIQUE constraint failed") || strContains(s, "UNIQUE violation")
}

func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
