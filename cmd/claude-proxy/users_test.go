package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

func cliDB(t *testing.T) (string, *store.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return path, db
}

func TestUsersListShowsEmail(t *testing.T) {
	ctx := context.Background()
	_, db := cliDB(t)
	if _, err := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := usertoken.Create(ctx, db, "ci"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printUsers(ctx, db, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "EMAIL") {
		t.Fatalf("list output:\n%s", out.String())
	}
	if !strings.Contains(lines[1], "alice@vocdoni.org") {
		t.Fatalf("alice row has no e-mail: %q", lines[1])
	}
	if !strings.Contains(lines[2], " - ") {
		t.Fatalf("ci row should show '-' for no e-mail: %q", lines[2])
	}
}

func TestUsersEmailRefusesDuplicate(t *testing.T) {
	ctx := context.Background()
	path, db := cliDB(t)
	alice, _ := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org")
	bob, _ := usertoken.Create(ctx, db, "bob")

	var stdout, stderr bytes.Buffer
	if code := usersEmail(ctx, []string{bob.ID, "alice@vocdoni.org", "--db", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("duplicate: exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "another user token") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := usersEmail(ctx, []string{bob.ID, "Bob@Vocdoni.org", "--db", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("set: exit %d, stderr %q", code, stderr.String())
	}
	got, _ := usertoken.Get(ctx, db, bob.ID)
	if got.Email != "bob@vocdoni.org" {
		t.Fatalf("e-mail = %q", got.Email)
	}
	if code := usersEmail(ctx, []string{alice.ID}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing argument: exit %d", code)
	}
}

func TestUsersSubagents(t *testing.T) {
	ctx := context.Background()
	path, db := cliDB(t)
	ut, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	list := func() []string {
		t.Helper()
		var out bytes.Buffer
		if err := printUsers(ctx, db, &out); err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(out.String()), "\n")
	}
	if lines := list(); !strings.Contains(lines[0], "SUBAGENTS") || !strings.Contains(lines[1], " spread ") {
		t.Fatalf("list output:\n%s", strings.Join(lines, "\n"))
	}

	var stdout, stderr bytes.Buffer
	if code := usersSubagents(ctx, []string{ut.ID, "off", "--db", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("off: exit %d, stderr %q", code, stderr.String())
	}
	if got, _ := usertoken.Get(ctx, db, ut.ID); got.SpreadSubagents {
		t.Fatal("off not persisted")
	}
	if lines := list(); !strings.Contains(lines[1], " share ") {
		t.Fatalf("row after off: %q", lines[1])
	}
	if code := usersSubagents(ctx, []string{ut.ID, "on", "--db", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("on: exit %d, stderr %q", code, stderr.String())
	}
	if got, _ := usertoken.Get(ctx, db, ut.ID); !got.SpreadSubagents {
		t.Fatal("on not persisted")
	}

	stderr.Reset()
	if code := usersSubagents(ctx, []string{"utok_missing", "on", "--db", path}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "not found") {
		t.Fatalf("unknown id: exit %d, stderr %q", code, stderr.String())
	}
	if code := usersSubagents(ctx, []string{ut.ID, "maybe", "--db", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad value: exit %d", code)
	}
	if code := usersSubagents(ctx, []string{ut.ID}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing value: exit %d", code)
	}
}
