package plugins

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var sha = strings.Repeat("ab", 32)

func goodMeta() Meta {
	return Meta{
		Version:      "1.4.0",
		BundleURL:    "https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip",
		BundleSHA256: sha,
		InstalledID:  "vocdoni-internal@vocdoni",
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	cases := []struct {
		name string
		mut  func(n *string, m *Meta)
		ok   bool
	}{
		{"valid", func(*string, *Meta) {}, true},
		{"name 40 chars", func(n *string, _ *Meta) { *n = strings.Repeat("a", 40) }, true},
		{"name 41 chars", func(n *string, _ *Meta) { *n = strings.Repeat("a", 41) }, false},
		{"name empty", func(n *string, _ *Meta) { *n = "" }, false},
		{"name upper", func(n *string, _ *Meta) { *n = "Vocdoni" }, false},
		{"name underscore", func(n *string, _ *Meta) { *n = "a_b" }, false},
		{"version 64", func(_ *string, m *Meta) { m.Version = strings.Repeat("v", 64) }, true},
		{"version 65", func(_ *string, m *Meta) { m.Version = strings.Repeat("v", 65) }, false},
		{"version empty", func(_ *string, m *Meta) { m.Version = "" }, false},
		{"version non-ascii", func(_ *string, m *Meta) { m.Version = "1.0-ü" }, false},
		{"version control", func(_ *string, m *Meta) { m.Version = "1.0\n" }, false},
		{"url http", func(_ *string, m *Meta) { m.BundleURL = "http://example.org/a.zip" }, false},
		{"url relative", func(_ *string, m *Meta) { m.BundleURL = "/a.zip" }, false},
		{"url userinfo", func(_ *string, m *Meta) { m.BundleURL = "https://u:p@example.org/a.zip" }, false},
		{"sha upper", func(_ *string, m *Meta) { m.BundleSHA256 = strings.ToUpper(sha) }, false},
		{"sha short", func(_ *string, m *Meta) { m.BundleSHA256 = sha[:63] }, false},
		{"installed empty", func(_ *string, m *Meta) { m.InstalledID = "" }, true},
		{"installed no at", func(_ *string, m *Meta) { m.InstalledID = "vocdoni-internal" }, false},
		{"installed upper", func(_ *string, m *Meta) { m.InstalledID = "A@b" }, false},
		{"installed long", func(_ *string, m *Meta) { m.InstalledID = strings.Repeat("a", 41) + "@b" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, m := "vocdoni-internal", goodMeta()
			tc.mut(&name, &m)
			err := Set(ctx, db, name, m)
			if tc.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestEnvValidation(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := Set(ctx, db, "p", goodMeta()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		key, value string
		ok         bool
	}{
		{"VOCDONI_KB_TOKEN", "x", true},
		{"_A", "x", true},
		{"A" + strings.Repeat("B", 63), "x", true},
		{"A" + strings.Repeat("B", 64), "x", false},
		{"lower", "x", false},
		{"1ABC", "x", false},
		{"A-B", "x", false},
		{"", "x", false},
		{"BIG", strings.Repeat("v", 4096), true},
		{"BIG", strings.Repeat("v", 4097), false},
		{"MULTI", "line one\nline two\tTabbed", true},
		{"NUL", "a\x00b", false},
		{"ESC", "a\x1bb", false},
	}
	for _, tc := range cases {
		err := SetEnv(ctx, db, "p", map[string]string{tc.key: tc.value})
		if tc.ok && err != nil {
			t.Errorf("%q (%d bytes) rejected: %v", tc.key, len(tc.value), err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%q (%d bytes): want ErrInvalid, got %v", tc.key, len(tc.value), err)
		}
	}
	// One bad key rejects the whole call: nothing is written.
	err := SetEnv(ctx, db, "p", map[string]string{"GOOD_NEW": "1", "bad": "2"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed batch: %v", err)
	}
	p, _ := Get(ctx, db, "p")
	for _, k := range p.EnvKeys {
		if k == "GOOD_NEW" {
			t.Fatal("partial write of a rejected batch")
		}
	}
	if err := SetEnv(ctx, db, "missing", map[string]string{"A": "1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("env on a missing plugin: %v", err)
	}
}

func TestCaps(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	for i := range MaxPlugins {
		if err := Set(ctx, db, fmt.Sprintf("p%d", i), goodMeta()); err != nil {
			t.Fatalf("plugin %d: %v", i, err)
		}
	}
	if err := Set(ctx, db, "one-too-many", goodMeta()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("33rd plugin: %v", err)
	}
	// Replacing an existing plugin's metadata is not a new plugin.
	if err := Set(ctx, db, "p0", goodMeta()); err != nil {
		t.Fatalf("update at the cap: %v", err)
	}

	env := make(map[string]string)
	for i := range MaxEnvKeys {
		env[fmt.Sprintf("K%d", i)] = "v"
	}
	if err := SetEnv(ctx, db, "p0", env); err != nil {
		t.Fatalf("64 keys: %v", err)
	}
	if err := SetEnv(ctx, db, "p0", map[string]string{"K0": "new"}); err != nil {
		t.Fatalf("overwrite at the cap: %v", err)
	}
	if err := SetEnv(ctx, db, "p0", map[string]string{"K64": "v"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("65th key: %v", err)
	}
	p, _ := Get(ctx, db, "p0")
	if len(p.EnvKeys) != MaxEnvKeys {
		t.Fatalf("keys after refused add = %d", len(p.EnvKeys))
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := Set(ctx, db, "vocdoni-internal", goodMeta()); err != nil {
		t.Fatal(err)
	}
	if err := SetEnv(ctx, db, "vocdoni-internal", map[string]string{"NOTION_TOKEN": "n-secret", "VOCDONI_KB_TOKEN": "kb-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := Set(ctx, db, "other", Meta{Version: "1", BundleURL: "https://example.org/o.zip", BundleSHA256: sha}); err != nil {
		t.Fatal(err)
	}

	list, err := List(ctx, db)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	vi := list[1]
	if vi.Name != "vocdoni-internal" || !vi.Enabled || vi.InstalledID != "vocdoni-internal@vocdoni" ||
		strings.Join(vi.EnvKeys, ",") != "NOTION_TOKEN,VOCDONI_KB_TOKEN" || vi.UpdatedAt.IsZero() {
		t.Fatalf("list entry: %+v", vi)
	}

	// Metadata replacement keeps env and the enabled flag.
	if err := Enable(ctx, db, "vocdoni-internal", false); err != nil {
		t.Fatal(err)
	}
	m := goodMeta()
	m.Version = "1.5.0"
	if err := Set(ctx, db, "vocdoni-internal", m); err != nil {
		t.Fatal(err)
	}
	got, err := Get(ctx, db, "vocdoni-internal")
	if err != nil || got.Version != "1.5.0" || got.Enabled || len(got.EnvKeys) != 2 {
		t.Fatalf("after replace: %v %+v", err, got)
	}

	// Disabled plugins are not served to clients.
	client, err := ListForClient(ctx, db)
	if err != nil || len(client) != 1 || client[0].Name != "other" || len(client[0].Env) != 0 {
		t.Fatalf("client list with vocdoni-internal disabled: %v %+v", err, client)
	}
	if err := Enable(ctx, db, "vocdoni-internal", true); err != nil {
		t.Fatal(err)
	}
	client, _ = ListForClient(ctx, db)
	if len(client) != 2 || client[1].Env["VOCDONI_KB_TOKEN"] != "kb-secret" || client[1].BundleSHA256 != sha {
		t.Fatalf("client list: %+v", client)
	}

	if err := UnsetEnv(ctx, db, "vocdoni-internal", "NOTION_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := UnsetEnv(ctx, db, "vocdoni-internal", "NOTION_TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset twice: %v", err)
	}
	if err := Delete(ctx, db, "vocdoni-internal"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM plugin_env`).Scan(&n)
	if n != 0 {
		t.Fatalf("env rows survive delete: %d", n)
	}
	for _, err := range []error{
		Delete(ctx, db, "vocdoni-internal"),
		Enable(ctx, db, "vocdoni-internal", true),
		UnsetEnv(ctx, db, "vocdoni-internal", "A"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing plugin: %v", err)
		}
	}
	if _, err := Get(ctx, db, "vocdoni-internal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}

func TestErrorsNeverCarryValues(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	_ = Set(ctx, db, "p", goodMeta())
	err := SetEnv(ctx, db, "p", map[string]string{"KEY": "super-secret\x00"})
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaks the value: %v", err)
	}
}
