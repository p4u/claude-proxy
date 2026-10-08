package creds

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusLimited  Status = "limited"
	StatusExpired  Status = "expired"
	StatusRevoked  Status = "revoked"
	StatusDisabled Status = "disabled"
)

type Credential struct {
	ID               string
	Label            string
	SubscriptionType string
	// RateLimitTier is descriptive metadata, not a routing weight. Empty means
	// unknown; preserve upstream values as well as operator labels like "20x".
	RateLimitTier string `json:"rate_limit_tier"`
	// Provider names the upstream this credential authenticates against.
	// Never empty for rows read back from the database (the column defaults to
	// "anthropic"), and provider.Get tolerates an empty value regardless.
	Provider provider.ID
	// BaseURL overrides the provider's default endpoint. Empty = default.
	// Required for provider.Custom, which has no default.
	BaseURL string
	// Models is this credential's own catalogue, used by custom hosts that
	// publish no /v1/models. Empty for registry providers, which either
	// discover their models or declare them statically.
	Models        []Model
	AccessToken   string
	RefreshToken  string
	ExpiresAt     time.Time
	Status        Status
	RetryAfter    *time.Time
	LastSuccessAt *time.Time
	Last429At     *time.Time
	LastRequestAt *time.Time
	RequestCount  int64
	SuccessCount  int64
	ErrorCount    int64
	Weight        int
	CreatedAt     time.Time
}

// Model is one entry in a credential's own catalogue.
//
// ContextWindow and MaxOutput are advisory and often unknown: they are only
// discoverable from a GET /v1/models the host may not serve, so they are
// omitted rather than guessed when absent.
type Model struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxOutput     int    `json:"max_output,omitempty"`
}

// encodeModels/decodeModels keep the catalogue in one TEXT column. A malformed
// value decodes to nil rather than failing the read: a credential with an
// unreadable catalogue should still be listable and deletable.
func encodeModels(ms []Model) string {
	if len(ms) == 0 {
		return ""
	}
	b, err := json.Marshal(ms)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeModels(raw string) []Model {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var ms []Model
	if err := json.Unmarshal([]byte(raw), &ms); err != nil {
		return nil
	}
	return ms
}

// SetModels replaces a credential's model catalogue.
func SetModels(ctx context.Context, db *store.DB, id string, ms []Model) error {
	res, err := db.ExecContext(ctx,
		`UPDATE credentials SET models=? WHERE id=?`, encodeModels(ms), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DefaultWeight returns the selection weight implied by a subscription tier.
// Higher = more new conversations routed here. Tweak freely — these are
// heuristics, not published numbers.
//
// Weights only ever compete within a single provider, because the provider is a
// hard filter applied before scoring: a GLM key is never weighed against an
// Anthropic subscription, only against other GLM keys. Providers whose plan
// tiers we cannot infer therefore start uniform at 1 and are differentiated by
// the operator with `creds set-weight`.
func DefaultWeight(p provider.ID, subscriptionType string) int {
	if p != "" && p != provider.Anthropic {
		return 1
	}
	switch subscriptionType {
	case "max":
		return 5
	case "team":
		return 5
	case "enterprise":
		return 5
	case "pro":
		return 1
	default:
		return 1
	}
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "cred_" + hex.EncodeToString(b[:])
}

// Insert stores an Anthropic OAuth credential (access + refresh token pair).
// For static API keys from other providers, use InsertKey.
func Insert(ctx context.Context, db *store.DB, label, subType, access, refresh string, expiresAt time.Time, weight int) (*Credential, error) {
	return InsertWithRateLimitTier(ctx, db, label, subType, access, refresh, expiresAt, weight, "")
}

// InsertWithRateLimitTier stores an OAuth credential and its known tier in one
// write. Insert remains the backward-compatible entry point for unknown tiers.
func InsertWithRateLimitTier(ctx context.Context, db *store.DB, label, subType, access, refresh string, expiresAt time.Time, weight int, tier string) (*Credential, error) {
	return insert(ctx, db, provider.Anthropic, "", nil, label, subType, access, refresh, expiresAt, weight, tier)
}

// keyExpiry is the stored expires_at for API-key credentials. They never
// expire, but the column is NOT NULL and the refresher compares against it, so
// a date far enough out that the proactive refresh window can never open is
// simpler — and less fragile — than threading a nullable expiry through every
// caller. The refresher also skips non-refreshable providers outright, so this
// is belt-and-braces.
func keyExpiry() time.Time { return time.Now().AddDate(100, 0, 0) }

// InsertKey stores a static API-key credential (GLM and any future key-based
// provider). There is no refresh token and no meaningful expiry.
func InsertKey(ctx context.Context, db *store.DB, p provider.ID, label, plan, apiKey, baseURL string, weight int) (*Credential, error) {
	return insert(ctx, db, p, baseURL, nil, label, plan, apiKey, "", keyExpiry(), weight, "")
}

// InsertCustomKey stores a key for a custom Anthropic-compatible host, which
// carries its own base URL and model catalogue.
func InsertCustomKey(ctx context.Context, db *store.DB, label, apiKey, baseURL string, models []Model, weight int) (*Credential, error) {
	return insert(ctx, db, provider.Custom, baseURL, models, label, "", apiKey, "", keyExpiry(), weight, "")
}

// InsertCustomOpenAIKey stores a bearer token for a custom OpenAI-compatible
// Chat Completions host. Like a custom Anthropic host, it carries its own base
// URL and model catalogue.
func InsertCustomOpenAIKey(ctx context.Context, db *store.DB, label, apiKey, baseURL string, models []Model, weight int) (*Credential, error) {
	return insert(ctx, db, provider.CustomOpenAI, baseURL, models, label, "", apiKey, "", keyExpiry(), weight, "")
}

func insert(ctx context.Context, db *store.DB, p provider.ID, baseURL string, models []Model, label, subType, access, refresh string, expiresAt time.Time, weight int, tier string) (*Credential, error) {
	tier, err := NormalizeRateLimitTier(tier)
	if err != nil {
		return nil, err
	}
	if p == "" {
		p = provider.Default
	}
	if weight < 1 {
		weight = DefaultWeight(p, subType)
	}
	c := &Credential{
		ID:               newID(),
		Label:            label,
		SubscriptionType: subType,
		RateLimitTier:    tier,
		Provider:         p,
		BaseURL:          baseURL,
		Models:           models,
		AccessToken:      access,
		RefreshToken:     refresh,
		ExpiresAt:        expiresAt,
		Status:           StatusActive,
		Weight:           weight,
		CreatedAt:        time.Now(),
	}
	_, err = db.ExecContext(
		ctx, `
		INSERT INTO credentials (id, label, subscription_type, rate_limit_tier, provider, base_url, models, access_token, refresh_token, expires_at, status, weight, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Label, c.SubscriptionType, c.RateLimitTier, string(c.Provider), c.BaseURL, encodeModels(c.Models), c.AccessToken, c.RefreshToken, c.ExpiresAt.Unix(), string(c.Status), c.Weight, c.CreatedAt.Unix(),
	)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ErrInvalidRateLimitTier identifies malformed tier metadata, distinct from a
// storage failure so callers can report a validation error without guessing.
var ErrInvalidRateLimitTier = errors.New("invalid rate limit tier")

// NormalizeRateLimitTier validates and trims a raw provider tier or manual
// label. The stored value is at most 128 UTF-8 bytes; empty means unknown.
// Control characters are rejected even at the edges, before trimming spaces.
func NormalizeRateLimitTier(tier string) (string, error) {
	if !utf8.ValidString(tier) {
		return "", fmt.Errorf("%w: must be valid UTF-8", ErrInvalidRateLimitTier)
	}
	if strings.ContainsFunc(tier, unicode.IsControl) {
		return "", fmt.Errorf("%w: must not contain control characters", ErrInvalidRateLimitTier)
	}
	tier = strings.TrimSpace(tier)
	if len(tier) > 128 {
		return "", fmt.Errorf("%w: must be at most 128 UTF-8 bytes", ErrInvalidRateLimitTier)
	}
	return tier, nil
}

// SetRateLimitTier replaces (or clears) descriptive metadata only. It never
// changes a credential's subscription type, routing weight, or status.
func SetRateLimitTier(ctx context.Context, db *store.DB, id, tier string) error {
	tier, err := NormalizeRateLimitTier(tier)
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `UPDATE credentials SET rate_limit_tier=? WHERE id=?`, tier, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetWeight updates the round-robin weight for a credential. weight must be >= 1.
func SetWeight(ctx context.Context, db *store.DB, id string, weight int) error {
	if weight < 1 {
		return errors.New("weight must be >= 1")
	}
	res, err := db.ExecContext(ctx, `UPDATE credentials SET weight=? WHERE id=?`, weight, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetBaseURL overrides (or clears, when empty) a credential's endpoint.
// Callers are expected to have verified the key against the new endpoint
// first — see ingest.UpdateKeyEndpoint.
func SetBaseURL(ctx context.Context, db *store.DB, id, baseURL string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE credentials SET base_url=? WHERE id=?`, strings.TrimSuffix(baseURL, "/"), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SelectCols is the credential column list, exported because internal/pool
// reads credentials inside its own transaction and must stay column-for-column
// identical to ScanCred below.
const SelectCols = `id, COALESCE(label,''), COALESCE(subscription_type,''), rate_limit_tier,
       COALESCE(provider,'anthropic'), COALESCE(base_url,''), COALESCE(models,''),
       access_token, refresh_token, expires_at, status,
       retry_after, last_success_at, last_429_at, last_request_at,
       request_count, success_count, error_count, weight, created_at`

const credSelectCols = SelectCols

// ScanCred materialises one row selected with SelectCols. Exported for the same
// reason: it keeps the pool's in-transaction read from drifting out of sync
// with this one, which is how a column addition silently breaks selection.
func ScanCred(rs interface {
	Scan(...any) error
},
) (*Credential, error) {
	return scanCred(rs)
}

func scanCred(rs interface {
	Scan(...any) error
},
) (*Credential, error) {
	c := &Credential{}
	var exp, created int64
	var ra, ls, l429, lreq sql.NullInt64
	var status, prov, models string
	if err := rs.Scan(
		&c.ID, &c.Label, &c.SubscriptionType, &c.RateLimitTier, &prov, &c.BaseURL, &models,
		&c.AccessToken, &c.RefreshToken, &exp, &status,
		&ra, &ls, &l429, &lreq,
		&c.RequestCount, &c.SuccessCount, &c.ErrorCount, &c.Weight, &created,
	); err != nil {
		return nil, err
	}
	c.Provider = provider.ID(prov)
	c.Models = decodeModels(models)
	c.ExpiresAt = time.Unix(exp, 0)
	c.Status = Status(status)
	c.CreatedAt = time.Unix(created, 0)
	if ra.Valid {
		t := time.Unix(ra.Int64, 0)
		c.RetryAfter = &t
	}
	if ls.Valid {
		t := time.Unix(ls.Int64, 0)
		c.LastSuccessAt = &t
	}
	if l429.Valid {
		t := time.Unix(l429.Int64, 0)
		c.Last429At = &t
	}
	if lreq.Valid {
		t := time.Unix(lreq.Int64, 0)
		c.LastRequestAt = &t
	}
	return c, nil
}

func List(ctx context.Context, db *store.DB) ([]*Credential, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+credSelectCols+` FROM credentials ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Credential
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasRefreshToken reports whether any credential in the DB already uses the
// given refresh token. Used during import to detect duplicates.
func HasRefreshToken(ctx context.Context, db *store.DB, refreshToken string) (bool, error) {
	var n int
	err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM credentials WHERE refresh_token=?`, refreshToken,
	).Scan(&n)
	return n > 0, err
}

// HasAccessToken reports whether any credential already stores the given
// access token. This is the duplicate check for API-key providers, where
// HasRefreshToken cannot work: key credentials store an empty refresh token,
// so every one of them would collide with every other.
func HasAccessToken(ctx context.Context, db *store.DB, accessToken string) (bool, error) {
	var n int
	err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM credentials WHERE access_token=?`, accessToken,
	).Scan(&n)
	return n > 0, err
}

func Get(ctx context.Context, db *store.DB, id string) (*Credential, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+credSelectCols+` FROM credentials WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scanCred(rows)
}

// Delete removes a credential and its dependent rows. The conversations table
// references credentials(id) without ON DELETE CASCADE (older databases were
// created that way and SQLite cannot alter a constraint in place), so we clear
// its sticky bindings inside a transaction before deleting the credential —
// otherwise the delete fails with FOREIGN KEY constraint failed (787).
// usage_history rows are removed automatically by its ON DELETE CASCADE;
// request_log keeps its historical credential_id (no FK) for stats.
func Delete(ctx context.Context, db *store.DB, id string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE credential_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func SetStatus(ctx context.Context, db *store.DB, id string, s Status) error {
	_, err := db.ExecContext(ctx, `UPDATE credentials SET status=? WHERE id=?`, string(s), id)
	return err
}

func UpdateTokens(ctx context.Context, db *store.DB, id, access, refresh string, expiresAt time.Time) error {
	_, err := db.ExecContext(ctx, `
		UPDATE credentials SET access_token=?, refresh_token=?, expires_at=?, status='active'
		WHERE id=?`, access, refresh, expiresAt.Unix(), id)
	return err
}

// UpdateTokensWithSubscription atomically reconnects a credential and records
// known subscription metadata. Absent values retain existing metadata, except
// that a changed broad plan clears a stale tier when no replacement is supplied.
// Weight is never derived from the tier or changed during a reconnect.
func UpdateTokensWithSubscription(ctx context.Context, db *store.DB, id, access, refresh string, expiresAt time.Time, subType, tier string) error {
	tier, err := NormalizeRateLimitTier(tier)
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `UPDATE credentials
		SET access_token=?, refresh_token=?, expires_at=?, status='active',
		    rate_limit_tier=CASE
		        WHEN ?<>'' THEN ?
		        WHEN ?<>'' AND ?<>COALESCE(subscription_type,'') THEN ''
		        ELSE rate_limit_tier END,
		    subscription_type=CASE WHEN ?<>'' THEN ? ELSE subscription_type END
		WHERE id=?`, access, refresh, expiresAt.Unix(), tier, tier, subType, subType, subType, subType, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSubscriptionType records the plan, e.g. after a reconnect verified that
// the account moved from pro to max. A changed plan invalidates the old tier;
// unchanged plans retain imported or manual labels. Routing weight is untouched.
func SetSubscriptionType(ctx context.Context, db *store.DB, id, subType string) error {
	_, err := db.ExecContext(ctx, `UPDATE credentials
		SET subscription_type=?, rate_limit_tier=CASE
			WHEN COALESCE(subscription_type,'')=? THEN rate_limit_tier ELSE '' END
		WHERE id=?`, subType, subType, id)
	return err
}

func MarkLimited(ctx context.Context, db *store.DB, id string, retryAfter time.Time) error {
	_, err := db.ExecContext(ctx, `
		UPDATE credentials SET status='limited', retry_after=?, last_429_at=? WHERE id=?`,
		retryAfter.Unix(), time.Now().Unix(), id)
	return err
}

func MarkSuccess(ctx context.Context, db *store.DB, id string) error {
	now := time.Now().Unix()
	_, err := db.ExecContext(ctx, `
		UPDATE credentials
		SET last_success_at=?,
		    success_count=success_count+1,
		    retry_after=CASE WHEN status='limited' THEN NULL ELSE retry_after END,
		    status=CASE WHEN status='limited' THEN 'active' ELSE status END
		WHERE id=?`, now, id)
	return err
}

// MarkRequest increments request_count and updates last_request_at. Called
// before forwarding so we count attempts (including ones that fail).
func MarkRequest(ctx context.Context, db *store.DB, id string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE credentials SET request_count=request_count+1, last_request_at=? WHERE id=?`,
		time.Now().Unix(), id)
	return err
}

// MarkError bumps the error counter (non-2xx, non-429-limited paths).
func MarkError(ctx context.Context, db *store.DB, id string) error {
	_, err := db.ExecContext(ctx, `UPDATE credentials SET error_count=error_count+1 WHERE id=?`, id)
	return err
}

var ErrNotFound = errors.New("credential not found")

// HasOATMarker is a sanity check for subscription OAuth tokens.
func HasOATMarker(token string) bool {
	return strings.Contains(token, "sk-ant-oat")
}
