package main

import "testing"

func TestParseDefaultLimit(t *testing.T) {
	for _, tc := range []struct {
		in             string
		tokens, window int64
		ok             bool
	}{
		{"", 0, 0, true},
		{"  ", 0, 0, true},
		{"0", 0, 0, true},
		{"--none", 0, 0, true},
		{"--tokens 1M --window 24h", 1_000_000, 86400, true},
		{" --tokens=500k --window=7d ", 500_000, 7 * 86400, true},
		{"--tokens 1M", 0, 0, false},
		{"--window 24h", 0, 0, false},
		{"--tokens 0 --window 24h", 0, 0, false},
		{"--tokens 1M --window 2h", 0, 0, false},
		{"--none --tokens 1M", 0, 0, false},
		{"--bogus", 0, 0, false},
		{"1M 24h", 0, 0, false},
	} {
		tokens, window, err := parseDefaultLimit(tc.in)
		if (err == nil) != tc.ok || tokens != tc.tokens || window != tc.window {
			t.Errorf("parseDefaultLimit(%q) = %d, %d, %v; want %d, %d, ok=%v",
				tc.in, tokens, window, err, tc.tokens, tc.window, tc.ok)
		}
	}
}
