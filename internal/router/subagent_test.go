package router

import (
	"strings"
	"testing"
)

// AC1: the agent id is accepted only as 1-128 characters of [0-9A-Za-z_-].
func TestValidAgentID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"a0123456789abcdef", true},
		{"aexplore-label-0123456789abcdef", true},
		{"hook-agent-3f2a9c1e-0b7d-4e55-9a10-2c4b6d8e0f12", true},
		{"A_b-9", true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{"", false},
		{"a/b", false},
		{"a b", false},
		{"a.b", false},
		{"a%2Fb", false},
		{"é", false},
	} {
		if got := ValidAgentID(tc.id); got != tc.want {
			t.Errorf("ValidAgentID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// AC1: a child key is the session key plus "/a_" and the shortened agent id,
// and sorts inside the family's primary-key range [parent+"/", parent+"0").
func TestSubagentKey(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"user_0123456789abcdef_account_acct_session_0b7d4e55"}}`)
	parent := Derive(req(""), body).ConvID
	if !strings.HasPrefix(parent, "u_") || len(parent) != len("u_")+16 {
		t.Fatalf("parent = %q, want a hashed u_ key", parent)
	}

	short := SubagentKey(parent, "a0123456789abcdef")
	if short != parent+"/a_a0123456789abcdef" {
		t.Fatalf("short id: %q", short)
	}
	long := strings.Repeat("x", 40)
	hashed := SubagentKey(parent, long)
	if hashed != parent+"/a_"+shorten(long) || len(hashed) != len(parent)+len("/a_")+16 {
		t.Fatalf("long id: %q", hashed)
	}
	if SubagentKey(parent, "a1") == SubagentKey(parent, "a2") {
		t.Fatal("distinct agent ids share a key")
	}
	for _, k := range []string{short, hashed} {
		if k < parent+"/" || k >= parent+"0" {
			t.Fatalf("%q is outside the family range of %q", k, parent)
		}
	}

	for _, tc := range []struct{ parent, agent string }{
		{parent, strings.Repeat("a", 129)},
		{parent, "a/b"},
		{parent, ""},
		{"u_short/raw", "a1"}, // a short raw metadata.user_id containing "/"
		{"", "a1"},
	} {
		if got := SubagentKey(tc.parent, tc.agent); got != "" {
			t.Errorf("SubagentKey(%q, %q) = %q, want \"\"", tc.parent, tc.agent, got)
		}
	}
}
