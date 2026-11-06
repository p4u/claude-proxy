// Package plugins stores the Claude Code plugins the proxy tells claudio to
// load: for each, a pinned bundle (URL and SHA-256 of a zip released from the
// content repo) and the environment its MCP servers and hooks need, secrets
// included.
//
// The proxy never fetches or serves the bundle. This package owns every
// validation limit; the CLI, the web UI routes and GET /v1/claudio/plugins
// all go through it. Env values are only ever returned by ListForClient.
package plugins

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

// Limits.
const (
	MaxPlugins    = 32
	MaxEnvKeys    = 64
	MaxValueBytes = 4096
	maxVersionLen = 64
	maxURLLen     = 2048
)

var (
	// ErrNotFound is returned for an unknown plugin or env key.
	ErrNotFound = errors.New("plugin not found")
	// ErrInvalid wraps every validation and limit error (a 400 for callers).
	ErrInvalid = errors.New("invalid plugin")

	nameRE      = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	installedRE = regexp.MustCompile(`^[a-z0-9-]{1,40}@[a-z0-9-]{1,40}$`)
	shaRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyRE       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
)

// Meta is a plugin's bundle reference, the part PUT /api/plugins/{name} and
// `plugins set` replace.
type Meta struct {
	Version      string
	BundleURL    string
	BundleSHA256 string
	InstalledID  string
}

// Plugin is a stored plugin as admins see it: env keys, never values.
type Plugin struct {
	Name string
	Meta
	Enabled   bool
	UpdatedAt time.Time
	EnvKeys   []string // sorted
}

// ClientPlugin is an enabled plugin as served to claudio, env values included.
type ClientPlugin struct {
	Name string
	Meta
	Env map[string]string
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidateName checks a plugin name: [a-z0-9-]{1,40}.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return invalid("name must match [a-z0-9-]{1,40}")
	}
	return nil
}

// Validate checks a bundle reference.
func (m Meta) Validate() error {
	if len(m.Version) < 1 || len(m.Version) > maxVersionLen || !printableASCII(m.Version) {
		return invalid("version must be 1-%d printable ASCII characters", maxVersionLen)
	}
	u, err := url.Parse(m.BundleURL)
	if len(m.BundleURL) > maxURLLen || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalid("bundle_url must be an https URL without credentials")
	}
	if !shaRE.MatchString(m.BundleSHA256) {
		return invalid("bundle_sha256 must be 64 lowercase hex characters")
	}
	if m.InstalledID != "" && !installedRE.MatchString(m.InstalledID) {
		return invalid("installed_id must be empty or name@marketplace ([a-z0-9-]{1,40} each)")
	}
	return nil
}

// ValidateEnv checks env keys and values. Errors name the key, never the value.
func ValidateEnv(env map[string]string) error {
	for k, v := range env {
		if !keyRE.MatchString(k) {
			return invalid("env key %q must match [A-Z_][A-Z0-9_]{0,63}", k)
		}
		if len(v) > MaxValueBytes {
			return invalid("env value of %s is over %d bytes", k, MaxValueBytes)
		}
		// claudio refuses a plugin whose values hold NUL or C0 controls other
		// than \n and \t; refuse them here so an admin finds out at save time.
		for i := 0; i < len(v); i++ {
			if c := v[i]; c < 0x20 && c != '\n' && c != '\t' {
				return invalid("env value of %s holds a control character", k)
			}
		}
	}
	return nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Set creates a plugin or replaces its bundle reference. A new plugin is
// enabled; an existing one keeps its enabled flag and env.
func Set(ctx context.Context, db *store.DB, name string, m Meta) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists, total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(name = ?), 0), COUNT(*) FROM plugins`, name).Scan(&exists, &total); err != nil {
		return err
	}
	if exists == 0 && total >= MaxPlugins {
		return invalid("at most %d plugins", MaxPlugins)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plugins (name, version, bundle_url, bundle_sha256, installed_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET version=excluded.version, bundle_url=excluded.bundle_url,
			bundle_sha256=excluded.bundle_sha256, installed_id=excluded.installed_id,
			updated_at=excluded.updated_at`,
		name, m.Version, m.BundleURL, m.BundleSHA256, m.InstalledID, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// Enable turns a plugin on or off for clients.
func Enable(ctx context.Context, db *store.DB, name string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := db.ExecContext(ctx, `UPDATE plugins SET enabled=?, updated_at=? WHERE name=?`,
		v, time.Now().Unix(), name)
	return affected(res, err)
}

// Delete removes a plugin and, by cascade, its env.
func Delete(ctx context.Context, db *store.DB, name string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM plugins WHERE name=?`, name)
	return affected(res, err)
}

// SetEnv upserts the listed keys of a plugin's env, leaving the others. The
// whole call is validated first and applied in one transaction.
func SetEnv(ctx context.Context, db *store.DB, name string, env map[string]string) error {
	if err := ValidateEnv(env); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE plugins SET updated_at=? WHERE name=?`, time.Now().Unix(), name)
	if err := affected(res, err); err != nil {
		return err
	}
	for k, v := range env {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO plugin_env (plugin, key, value) VALUES (?, ?, ?)
			ON CONFLICT(plugin, key) DO UPDATE SET value=excluded.value`, name, k, v); err != nil {
			return err
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_env WHERE plugin=?`, name).Scan(&n); err != nil {
		return err
	}
	if n > MaxEnvKeys {
		return invalid("at most %d env keys per plugin", MaxEnvKeys)
	}
	return tx.Commit()
}

// UnsetEnv removes env keys of a plugin. ErrNotFound when the plugin is
// unknown or none of the keys existed.
func UnsetEnv(ctx context.Context, db *store.DB, name string, keys ...string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var removed int64
	for _, k := range keys {
		res, err := tx.ExecContext(ctx, `DELETE FROM plugin_env WHERE plugin=? AND key=?`, name, k)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	if removed == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugins SET updated_at=? WHERE name=?`, time.Now().Unix(), name); err != nil {
		return err
	}
	return tx.Commit()
}

// Get returns one plugin with its env keys.
func Get(ctx context.Context, db *store.DB, name string) (*Plugin, error) {
	found, err := list(ctx, db, name)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return &found[0], nil
}

// List returns every plugin, by name, with env keys and no values.
func List(ctx context.Context, db *store.DB) ([]Plugin, error) {
	return list(ctx, db, "")
}

// list reads the plugins (all when only is "") and their env keys.
func list(ctx context.Context, db *store.DB, only string) ([]Plugin, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name, version, bundle_url, bundle_sha256, installed_id, enabled, updated_at
		FROM plugins WHERE ? = '' OR name = ? ORDER BY name`, only, only)
	if err != nil {
		return nil, err
	}
	var out []Plugin
	idx := make(map[string]int)
	for rows.Next() {
		var p Plugin
		var updated int64
		if err := rows.Scan(&p.Name, &p.Version, &p.BundleURL, &p.BundleSHA256, &p.InstalledID,
			&p.Enabled, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		p.UpdatedAt = time.Unix(updated, 0)
		p.EnvKeys = []string{}
		idx[p.Name] = len(out)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys, err := db.QueryContext(ctx,
		`SELECT plugin, key FROM plugin_env WHERE ? = '' OR plugin = ? ORDER BY plugin, key`, only, only)
	if err != nil {
		return nil, err
	}
	defer keys.Close()
	for keys.Next() {
		var plugin, key string
		if err := keys.Scan(&plugin, &key); err != nil {
			return nil, err
		}
		if i, ok := idx[plugin]; ok {
			out[i].EnvKeys = append(out[i].EnvKeys, key)
		}
	}
	return out, keys.Err()
}

// ListForClient returns the enabled plugins, by name, with their env values.
// Only GET /v1/claudio/plugins may call it.
func ListForClient(ctx context.Context, db *store.DB) ([]ClientPlugin, error) {
	all, err := List(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]ClientPlugin, 0, len(all))
	idx := make(map[string]int)
	for _, p := range all {
		if !p.Enabled {
			continue
		}
		idx[p.Name] = len(out)
		out = append(out, ClientPlugin{Name: p.Name, Meta: p.Meta, Env: map[string]string{}})
	}
	rows, err := db.QueryContext(ctx, `SELECT plugin, key, value FROM plugin_env`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var plugin, key, value string
		if err := rows.Scan(&plugin, &key, &value); err != nil {
			return nil, err
		}
		if i, ok := idx[plugin]; ok {
			out[i].Env[key] = value
		}
	}
	return out, rows.Err()
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
