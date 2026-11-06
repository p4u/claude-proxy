package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/plugins"
)

// runCLI runs `claude-proxy plugins <args> --db path` and returns the exit
// code, stdout and stderr.
func runCLI(t *testing.T, path, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runPlugins(append(args, "--db", path), strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestPluginsCLIRoundTrip(t *testing.T) {
	ctx := context.Background()
	path, db := cliDB(t)
	sha := strings.Repeat("ab", 32)
	secrets := []string{"kb-secret", "notion-secret", "file-secret", "argv-secret"}
	var out strings.Builder
	run := func(stdin string, args ...string) {
		t.Helper()
		code, stdout, stderr := runCLI(t, path, stdin, args...)
		if code != 0 {
			t.Fatalf("plugins %v: exit %d, stderr %q", args, code, stderr)
		}
		out.WriteString(stdout + stderr)
	}

	run("", "set", "vocdoni-internal", "--version", "1.4.0",
		"--url", "https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip",
		"--sha256", sha, "--installed-id", "vocdoni-internal@vocdoni")
	run("kb-secret\nnotion-secret\n", "env", "set", "vocdoni-internal", "VOCDONI_KB_TOKEN=-", "NOTION_TOKEN=-")

	envFile := filepath.Join(t.TempDir(), "plugin.env")
	if err := os.WriteFile(envFile, []byte("# comment\n\nFILE_TOKEN=\"file-secret\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("", "env", "set", "vocdoni-internal", "--env-file", envFile)
	run("", "env", "set", "vocdoni-internal", "ARGV_TOKEN=argv-secret")

	client, err := plugins.ListForClient(ctx, db)
	if err != nil || len(client) != 1 {
		t.Fatalf("client list: %v %+v", err, client)
	}
	env := client[0].Env
	if env["VOCDONI_KB_TOKEN"] != "kb-secret" || env["NOTION_TOKEN"] != "notion-secret" ||
		env["FILE_TOKEN"] != "file-secret" || env["ARGV_TOKEN"] != "argv-secret" {
		t.Fatalf("env = %v", env)
	}

	code, list, _ := runCLI(t, path, "", "list")
	if code != 0 || !strings.Contains(list, "vocdoni-internal") || !strings.Contains(list, "1.4.0") ||
		!strings.Contains(list, "vocdoni-internal@vocdoni") || !strings.Contains(list, "on") {
		t.Fatalf("list:\n%s", list)
	}
	out.WriteString(list)
	code, keys, _ := runCLI(t, path, "", "env", "list", "vocdoni-internal")
	if code != 0 || strings.TrimSpace(keys) != "ARGV_TOKEN\nFILE_TOKEN\nNOTION_TOKEN\nVOCDONI_KB_TOKEN" {
		t.Fatalf("env list:\n%s", keys)
	}
	out.WriteString(keys)

	run("", "env", "unset", "vocdoni-internal", "ARGV_TOKEN", "FILE_TOKEN")
	run("", "disable", "vocdoni-internal")
	if client, _ := plugins.ListForClient(ctx, db); len(client) != 0 {
		t.Fatal("disabled plugin still served")
	}
	run("", "enable", "vocdoni-internal")
	p, _ := plugins.Get(ctx, db, "vocdoni-internal")
	if !p.Enabled || strings.Join(p.EnvKeys, ",") != "NOTION_TOKEN,VOCDONI_KB_TOKEN" {
		t.Fatalf("after unset/enable: %+v", p)
	}
	run("", "delete", "vocdoni-internal")
	if _, err := plugins.Get(ctx, db, "vocdoni-internal"); err != plugins.ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}

	for _, s := range secrets {
		if strings.Contains(out.String(), s) {
			t.Fatalf("CLI output shows a secret value %q:\n%s", s, out.String())
		}
	}
}

func TestPluginsCLIErrors(t *testing.T) {
	path, _ := cliDB(t)
	sha := strings.Repeat("ab", 32)
	var stderr bytes.Buffer
	if code := runPlugins(nil, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("no subcommand: exit %d stderr %q", code, stderr.String())
	}
	for _, tc := range []struct {
		stdin string
		args  []string
		code  int
		msg   string
	}{
		{"", []string{"bogus"}, 2, "unknown subcommand"},
		{"", []string{"set", "p", "--version", "1", "--url", "http://x.test/a.zip", "--sha256", sha}, 1, "https"},
		{"", []string{"set", "p", "--url", "https://x.test/a.zip", "--sha256", sha}, 2, "--version"},
		{"", []string{"enable", "missing"}, 1, "not found"},
		{"x\n", []string{"env", "set", "missing", "A=-"}, 1, "not found"},
		{"one\n", []string{"env", "set", "p", "A=-", "B=-"}, 1, "stdin"},
		{"", []string{"env", "set", "p", "lower=1"}, 1, "env key"},
		{"", []string{"env", "set", "p", "NOEQUALS"}, 2, "KEY=VALUE"},
		{"", []string{"env", "set", "p"}, 2, "no variables"},
		{"", []string{"env", "list"}, 2, "plugins env list <name>"},
	} {
		code, _, stderr := runCLI(t, path, tc.stdin, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.msg) {
			t.Errorf("plugins %v: exit %d stderr %q, want exit %d containing %q", tc.args, code, stderr, tc.code, tc.msg)
		}
	}
}
