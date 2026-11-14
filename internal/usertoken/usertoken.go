package usertoken

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

var ErrNotFound = errors.New("user token not found")

// ErrEmailTaken is returned when an e-mail is already on another user token.
var ErrEmailTaken = errors.New("e-mail already belongs to another user token")

// columns is the SELECT list scan reads, in order.
const columns = `id, name, token, status, created_at, last_used_at, full_capture,
		       limit_output_tokens, limit_window_seconds, block_suggestions, email,
		       spread_subagents`

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
	// FullCapture opts this user into whole-conversation capture (both roles,
	// stored in conversation_message). Default false = last-user-prompt only.
	FullCapture bool
	// LimitOutputTokens / LimitWindowSeconds cap the user's output tokens over
	// a rolling window. A limit is active only when BOTH are > 0.
	LimitOutputTokens  int64
	LimitWindowSeconds int64
	// BlockSuggestions opts this user out of Claude Code's prompt-suggestion
	// traffic: the proxy answers those requests itself instead of forwarding
	// them. Default false = forward, matching pre-existing behaviour.
	BlockSuggestions bool
	// Email is the member this token belongs to (lower-cased); empty for
	// admin-created tokens.
	Email string
	// SpreadSubagents gives each Claude Code subagent its own binding, on a
	// subscription its session is not using when one has room. Default true.
	SpreadSubagents bool
}

// contextKey is unexported to prevent collisions across packages.
type contextKey int

const identityKey contextKey = iota

// Identity is injected into the request context by the auth middleware.
type Identity struct {
	IsAdmin     bool   // matched the master PROXY_AUTH_TOKEN
	UserTokenID string // non-empty when a user token was matched
	UserName    string
	FullCapture bool // user opted into whole-conversation capture
	// Usage limit carried through from the token row so enforcement needs no
	// extra lookup. Both > 0 => a limit is active.
	LimitOutputTokens  int64
	LimitWindowSeconds int64
	// BlockSuggestions => answer Claude Code's prompt-suggestion requests
	// locally instead of forwarding them upstream.
	BlockSuggestions bool
	// SpreadSubagents is the user's switch; read it through SpreadSubagentsFor.
	SpreadSubagents bool
}

// SpreadSubagentsFor reports whether a caller's subagent requests get their
// own bindings. It is on for a deployment without auth (nil) and for the
// admin token; a user token carries its own switch.
func SpreadSubagentsFor(id *Identity) bool {
	if id == nil || id.IsAdmin {
		return true
	}
	return id.SpreadSubagents
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
	return insert(ctx, db, name, "")
}

// CreateWithEmail inserts a new user token that belongs to email. It returns
// ErrEmailTaken when the address is already on another token.
func CreateWithEmail(ctx context.Context, db *store.DB, name, email string) (*UserToken, error) {
	email, err := checkEmail(email)
	if err != nil {
		return nil, err
	}
	return insert(ctx, db, name, email)
}

// maxNameSuffix bounds CreateForEmail's search for a free name.
const maxNameSuffix = 100

// CreateForEmail inserts a user token for a member who signed in with email.
// The name is the e-mail; when an admin already used that name, "-2", "-3",
// ... is appended (the e-mail, not the name, is the identity). It returns
// ErrEmailTaken when the address is already on a token.
func CreateForEmail(ctx context.Context, db *store.DB, email string) (*UserToken, error) {
	email, err := checkEmail(email)
	if err != nil {
		return nil, err
	}
	for i := 1; i <= maxNameSuffix; i++ {
		name := email
		if i > 1 {
			name = fmt.Sprintf("%s-%d", email, i)
		}
		ut, err := insert(ctx, db, name, email)
		if err == nil || !isUniqueOn(err, "name") {
			return ut, err
		}
	}
	return nil, fmt.Errorf("no free user name for %s", email)
}

// insert stores a new active token; email "" is stored as NULL.
func insert(ctx context.Context, db *store.DB, name, email string) (*UserToken, error) {
	ut := &UserToken{
		ID:              newID(),
		Name:            name,
		Token:           GenerateToken(),
		Status:          StatusActive,
		CreatedAt:       time.Now(),
		Email:           email,
		SpreadSubagents: true, // the column's default
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO user_tokens (id, name, token, status, created_at, email)
		VALUES (?, ?, ?, ?, ?, ?)`,
		ut.ID, ut.Name, ut.Token, string(ut.Status), ut.CreatedAt.Unix(), nullable(email))
	if err != nil {
		if isUniqueOn(err, "email") {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	return ut, nil
}

// NormalizeEmail is the stored form of an e-mail: trimmed and lower-cased.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// checkEmail normalises s and rejects anything that is not local@domain
// without spaces or control characters.
func checkEmail(s string) (string, error) {
	e := NormalizeEmail(s)
	at := strings.LastIndexByte(e, '@')
	if at <= 0 || at == len(e)-1 || len(e) > 254 {
		return "", fmt.Errorf("invalid e-mail %q", s)
	}
	for i := 0; i < len(e); i++ {
		if e[i] <= ' ' || e[i] == 0x7f {
			return "", fmt.Errorf("invalid e-mail %q", s)
		}
	}
	return e, nil
}

// isUniqueOn reports whether err is SQLite's UNIQUE violation on column.
func isUniqueOn(err error, column string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: user_tokens."+column)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// List returns all user tokens ordered by creation time.
func List(ctx context.Context, db *store.DB) ([]*UserToken, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens ORDER BY created_at`)
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
	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens WHERE id=?`, id)
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
	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens WHERE token=?`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scan(rows)
}

// LookupByEmail finds the user token that belongs to email.
func LookupByEmail(ctx context.Context, db *store.DB, email string) (*UserToken, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens WHERE email=?`, NormalizeEmail(email))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scan(rows)
}

// SetEmail sets the e-mail a token belongs to. It returns ErrEmailTaken when
// the address is already on another token.
func SetEmail(ctx context.Context, db *store.DB, id, email string) error {
	e, err := checkEmail(email)
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `UPDATE user_tokens SET email=? WHERE id=?`, e, id)
	if err != nil {
		if isUniqueOn(err, "email") {
			return ErrEmailTaken
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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

// SetFullCapture toggles whole-conversation capture for a user.
func SetFullCapture(ctx context.Context, db *store.DB, id string, full bool) error {
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET full_capture=? WHERE id=?`, boolToInt(full), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetBlockSuggestions toggles local handling of prompt-suggestion requests.
func SetBlockSuggestions(ctx context.Context, db *store.DB, id string, block bool) error {
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET block_suggestions=? WHERE id=?`, boolToInt(block), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSpreadSubagents turns subagent spreading on or off for a user.
func SetSpreadSubagents(ctx context.Context, db *store.DB, id string, on bool) error {
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET spread_subagents=? WHERE id=?`, boolToInt(on), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	var email sql.NullString
	var status string
	if err := rs.Scan(&ut.ID, &ut.Name, &ut.Token, &status, &created, &lu, &ut.FullCapture,
		&ut.LimitOutputTokens, &ut.LimitWindowSeconds, &ut.BlockSuggestions, &email,
		&ut.SpreadSubagents); err != nil {
		return nil, err
	}
	ut.Status = Status(status)
	ut.Email = email.String
	ut.CreatedAt = time.Unix(created, 0)
	if lu.Valid {
		t := time.Unix(lu.Int64, 0)
		ut.LastUsedAt = &t
	}
	return ut, nil
}
