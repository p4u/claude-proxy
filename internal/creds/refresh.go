package creds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

const ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// TokenURL is a var (not const) so tests can rewrite it.
var TokenURL = "https://platform.claude.com/v1/oauth/token"

const (
	// refreshWindow is how close to its expiry an access token gets refreshed.
	refreshWindow = 5 * time.Minute
	// LongLived marks a long-lived login: its access token outlives this when
	// minted, which an 8-hour token never does.
	LongLived = 24 * time.Hour
	// failureMemory is how long a transient refresh error is returned to later
	// callers without contacting the token endpoint again.
	failureMemory = 30 * time.Second
	// failureBackoff keeps new conversations off a credential whose refresh
	// just failed transiently (retry_after); existing pins keep it.
	failureBackoff = 60 * time.Second
	maxTokenBody   = 1 << 20
	maxQuotedBody  = 512
)

var (
	// ErrLoginRejected means the token endpoint answered 400/401 to the
	// refresh token and the credential was marked revoked.
	ErrLoginRejected = errors.New("login rejected by Anthropic")
	// ErrTokenAlive means RefreshRejected did not refresh: the access token
	// still works, or the liveness check could not prove it dead.
	ErrTokenAlive = errors.New("access token still valid")
	// ErrNotRefreshable means the credential's provider has no OAuth refresh.
	ErrNotRefreshable = errors.New("credential has no OAuth refresh")
)

type Refresher struct {
	db       *store.DB
	client   *http.Client
	log      *slog.Logger
	mus      sync.Map // id -> *sync.Mutex
	failures sync.Map // id -> failure
}

// failure is a credential's last transient refresh error.
type failure struct {
	err error
	at  time.Time
}

func NewRefresher(db *store.DB) *Refresher {
	return &Refresher{
		db:     db,
		client: &http.Client{Timeout: 30 * time.Second},
		log:    slog.New(slog.DiscardHandler),
	}
}

// SetLogger sets where refresh outcomes are logged; without it they are
// discarded. Call it before Loop or any refresh.
func (r *Refresher) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l
	}
}

// SetTokenClient swaps the HTTP client used for refresh requests (test hook).
func SetTokenClient(r *Refresher, c *http.Client) { r.client = c }

// SetTokenURL overrides the global token endpoint (test hook).
func SetTokenURL(u string) { TokenURL = u }

func (r *Refresher) lockFor(id string) *sync.Mutex {
	v, _ := r.mus.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type refreshReq struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

type refreshResp struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope,omitempty"`
	Error                 string `json:"error,omitempty"`
}

// TokenSet is what a refresh yields.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	// RefreshExpiresAt is the login's refresh deadline; zero when unknown.
	RefreshExpiresAt time.Time
}

var importClient = &http.Client{Timeout: 30 * time.Second}

// RefreshTokens performs a stateless refresh (no DB record required), used
// during import to verify a credential is alive before storing it. deadline
// is the login's known refresh deadline, zero when unknown; the result
// carries it updated by the same rule as a stored refresh.
func RefreshTokens(ctx context.Context, refreshToken string, deadline time.Time) (TokenSet, error) {
	now := time.Now()
	status, raw, err := tokenRequest(ctx, importClient, refreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		return TokenSet{}, fmt.Errorf("credential rejected by Anthropic (%d): %s", status, quoteBody(raw))
	}
	if status >= 300 {
		return TokenSet{}, fmt.Errorf("refresh upstream %d: %s", status, quoteBody(raw))
	}
	if status != http.StatusOK {
		return TokenSet{}, fmt.Errorf("refresh upstream %d: unexpected success status", status)
	}
	ts, err := decodeTokens(raw, now)
	if err != nil {
		return TokenSet{}, err
	}
	ts.RefreshExpiresAt = nextDeadline(deadline, ts.RefreshExpiresAt, now)
	return ts, nil
}

// tokenRequest posts a refresh_token grant and returns the status and body,
// read through a 1 MiB cap.
func tokenRequest(ctx context.Context, client *http.Client, refreshToken string) (int, []byte, error) {
	body, _ := json.Marshal(refreshReq{GrantType: "refresh_token", ClientID: ClientID, RefreshToken: refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return 0, nil, fmt.Errorf("refresh read: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// decodeTokens parses a 200 token response. RefreshExpiresAt is the deadline
// the response reports, before nextDeadline. Errors never quote the body: it
// carries the tokens.
func decodeTokens(raw []byte, now time.Time) (TokenSet, error) {
	var rr refreshResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return TokenSet{}, errors.New("refresh response is not a token response")
	}
	if rr.AccessToken == "" || rr.RefreshToken == "" {
		return TokenSet{}, errors.New("refresh response missing tokens")
	}
	ts := TokenSet{
		AccessToken:  rr.AccessToken,
		RefreshToken: rr.RefreshToken,
		ExpiresAt:    now.Add(time.Duration(rr.ExpiresIn)*time.Second - 5*time.Minute),
	}
	if rr.RefreshTokenExpiresIn > 0 {
		ts.RefreshExpiresAt = now.Add(time.Duration(rr.RefreshTokenExpiresIn) * time.Second)
	}
	return ts, nil
}

// nextDeadline is Claude Code's rule for the deadline after a refresh: a known
// deadline only ever moves earlier, and an unknown one stays unknown, because
// for older logins the returned value is not a deadline. A stored deadline
// that has passed while the refresh still succeeded was wrong, so the returned
// one (or unknown) replaces it.
func nextDeadline(stored, returned, now time.Time) time.Time {
	switch {
	case stored.IsZero():
		return time.Time{}
	case !stored.After(now):
		return returned
	case !returned.IsZero() && returned.Before(stored):
		return returned
	}
	return stored
}

// quoteBody trims a non-2xx token-endpoint body (OAuth error JSON) for an
// error or a log line. A 2xx body carries tokens and must never be passed here.
func quoteBody(raw []byte) string {
	if len(raw) > maxQuotedBody {
		raw = raw[:maxQuotedBody]
	}
	return string(raw)
}

// deadlineAttr renders a deadline for a log line.
func deadlineAttr(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// Refresh refreshes a credential whose access token expires within 5 minutes,
// or one marked revoked. Any other credential is returned unchanged with
// refreshed=false. Safe to call concurrently: serialised per id.
func (r *Refresher) Refresh(ctx context.Context, id string) (c *Credential, refreshed bool, err error) {
	mu := r.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	// From here on the work completes even if the caller goes away: a refresh
	// Anthropic has answered must be stored, or the rotated token is lost.
	ctx = context.WithoutCancel(ctx)
	c, err = Get(ctx, r.db, id)
	if err != nil {
		return nil, false, err
	}
	if !provider.Get(c.Provider).Refreshable {
		return c, false, ErrNotRefreshable
	}
	if time.Until(c.ExpiresAt) > refreshWindow && c.Status != StatusRevoked {
		return c, false, nil
	}
	return r.refreshLocked(ctx, c)
}

// RefreshRejected handles an upstream 401 on rejected, the access token the
// request carried. It refreshes only when the token is really dead: another
// request may already have replaced it, and a 401 can describe the request
// rather than the token, so a token far from expiry is first checked against
// the profile endpoint. ErrTokenAlive means nothing was refreshed. That verdict
// is not cached, so each 401 on a live token pays one serialised profile check
// (bounded 1:1 by the 401s).
func (r *Refresher) RefreshRejected(ctx context.Context, id, rejected string) (c *Credential, refreshed bool, err error) {
	mu := r.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	c, err = Get(ctx, r.db, id)
	if err != nil {
		return nil, false, err
	}
	if !provider.Get(c.Provider).Refreshable {
		return c, false, ErrNotRefreshable
	}
	if c.AccessToken != rejected {
		return c, false, nil
	}
	if time.Until(c.ExpiresAt) > refreshWindow {
		switch err := CheckToken(ctx, c.AccessToken); {
		case err == nil:
			return c, false, ErrTokenAlive
		case !errors.Is(err, ErrTokenRejected):
			return c, false, fmt.Errorf("%w: %w", ErrTokenAlive, err)
		case time.Until(c.ExpiresAt) > LongLived:
			r.log.Error("long-lived token rejected before its expiry",
				"cred", c.ID, "label", c.Label,
				"expires_at", c.ExpiresAt.UTC().Format(time.RFC3339),
				"refresh_deadline", deadlineAttr(c.RefreshExpiresAt))
		}
	}
	return r.refreshLocked(ctx, c)
}

// refreshLocked calls the token endpoint for c and stores the outcome. The
// caller holds c's mutex and passes a context that is never cancelled.
func (r *Refresher) refreshLocked(ctx context.Context, c *Credential) (*Credential, bool, error) {
	if err := r.recentFailure(c.ID); err != nil {
		return nil, false, err
	}
	now := time.Now()
	status, raw, err := tokenRequest(ctx, r.client, c.RefreshToken)
	if err == nil && (status == http.StatusBadRequest || status == http.StatusUnauthorized) {
		return r.rejected(ctx, c, status, raw)
	}
	var ts TokenSet
	switch {
	case err != nil:
	case status >= 300:
		err = fmt.Errorf("refresh upstream %d: %s", status, quoteBody(raw))
	case status != http.StatusOK:
		err = fmt.Errorf("refresh upstream %d: unexpected success status", status)
	default:
		ts, err = decodeTokens(raw, now)
	}
	if err != nil {
		r.transientFailure(ctx, c, err)
		return nil, false, err
	}

	returned := ts.RefreshExpiresAt
	ts.RefreshExpiresAt = nextDeadline(c.RefreshExpiresAt, returned, now)
	var stored bool
	err = store.Retry(ctx, func() error {
		var werr error
		stored, werr = replaceTokensIfLineage(ctx, r.db, c.ID, c.RefreshToken, ts)
		return werr
	})
	if err != nil {
		r.log.Error("refreshed upstream but not stored; the credential will be rejected at its next refresh",
			"cred", c.ID, "label", c.Label, "err", err)
		return nil, false, err
	}
	r.failures.Delete(c.ID)
	if !stored {
		// A reconnect replaced the tokens while this request was in flight.
		r.log.Debug("token refresh superseded by a newer login", "cred", c.ID, "label", c.Label)
		fresh, err := Get(ctx, r.db, c.ID)
		return fresh, false, err
	}
	r.log.Debug("token refreshed", "cred", c.ID, "label", c.Label,
		"expires_at", ts.ExpiresAt.UTC().Format(time.RFC3339),
		"refresh_deadline", deadlineAttr(ts.RefreshExpiresAt),
		"returned_deadline", deadlineAttr(returned))
	fresh, err := Get(ctx, r.db, c.ID)
	return fresh, true, err
}

// rejected handles a 400/401 from the token endpoint: the lineage is dead, so
// the credential is revoked, unless a newer login replaced it meanwhile.
func (r *Refresher) rejected(ctx context.Context, c *Credential, status int, raw []byte) (*Credential, bool, error) {
	var revoked bool
	err := store.Retry(ctx, func() error {
		var werr error
		revoked, werr = revokeIfLineage(ctx, r.db, c.ID, c.RefreshToken)
		return werr
	})
	if err != nil {
		r.log.Warn("token refresh rejected but the revocation was not stored",
			"cred", c.ID, "label", c.Label, "err", err)
		return nil, false, err
	}
	r.failures.Delete(c.ID)
	if !revoked {
		r.log.Debug("token refresh superseded by a newer login", "cred", c.ID, "label", c.Label)
		fresh, err := Get(ctx, r.db, c.ID)
		return fresh, false, err
	}
	passed := !c.RefreshExpiresAt.IsZero() && !c.RefreshExpiresAt.After(time.Now())
	r.log.Error("login rejected by Anthropic; credential revoked",
		"cred", c.ID, "label", c.Label, "status", status,
		"refresh_deadline", deadlineAttr(c.RefreshExpiresAt), "deadline_passed", passed,
		"upstream", quoteBody(raw))
	return nil, false, fmt.Errorf("%w (%d): %s", ErrLoginRejected, status, quoteBody(raw))
}

// transientFailure remembers err so callers queued on the mutex fail fast,
// and keeps new conversations off the credential for failureBackoff. The
// status is left alone: nothing proved the login dead.
func (r *Refresher) transientFailure(ctx context.Context, c *Credential, err error) {
	now := time.Now()
	r.failures.Store(c.ID, failure{err: err, at: now})
	werr := store.Retry(ctx, func() error {
		_, e := r.db.ExecContext(ctx,
			`UPDATE credentials SET retry_after=MAX(COALESCE(retry_after,0),?) WHERE id=?`,
			now.Add(failureBackoff).Unix(), c.ID)
		return e
	})
	r.log.Warn("token refresh failed", "cred", c.ID, "label", c.Label, "err", err,
		"retry_after_stored", werr == nil)
}

// recentFailure returns the credential's last transient error while it is
// younger than failureMemory.
func (r *Refresher) recentFailure(id string) error {
	v, ok := r.failures.Load(id)
	if !ok {
		return nil
	}
	f := v.(failure)
	if time.Since(f.at) >= failureMemory {
		r.failures.Delete(id)
		return nil
	}
	return f.err
}

// replaceTokensIfLineage writes a refresh result only while the credential
// still holds the refresh token the request was made with. It heals only
// token-related states: disabled and limited stay as they are.
func replaceTokensIfLineage(ctx context.Context, db *store.DB, id, oldRefresh string, ts TokenSet) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE credentials
		SET access_token=?, refresh_token=?, expires_at=?, refresh_expires_at=?,
		    status=CASE WHEN status IN ('revoked','expired') THEN 'active' ELSE status END
		WHERE id=? AND refresh_token=?`,
		ts.AccessToken, ts.RefreshToken, ts.ExpiresAt.Unix(), unixOrZero(ts.RefreshExpiresAt), id, oldRefresh)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// revokeIfLineage marks the credential revoked only while it still holds the
// refresh token that was rejected.
func revokeIfLineage(ctx context.Context, db *store.DB, id, oldRefresh string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE credentials SET status=? WHERE id=? AND refresh_token=?`, string(StatusRevoked), id, oldRefresh)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Loop runs proactive refresh in the background until ctx is cancelled.
func (r *Refresher) Loop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Refresher) tick(ctx context.Context) {
	creds, err := List(ctx, r.db)
	if err != nil {
		r.log.Warn("refresh loop: list credentials", "err", err)
		return
	}
	cutoff := time.Now().Add(refreshWindow)
	for _, c := range creds {
		// Static API keys have no refresh token and no real expiry.
		if !provider.Get(c.Provider).Refreshable {
			continue
		}
		if c.Status == StatusRevoked || c.Status == StatusDisabled {
			continue
		}
		if c.ExpiresAt.Before(cutoff) {
			_, _, _ = r.Refresh(ctx, c.ID) // refresh logs its own outcome
		}
	}
}
