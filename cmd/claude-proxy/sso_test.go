package main

import (
	"reflect"
	"testing"
)

func TestSplitList(t *testing.T) {
	for in, want := range map[string][]string{
		"":                               nil,
		" , ,":                           nil,
		"vocdoni.org":                    {"vocdoni.org"},
		" a@vocdoni.org , b@vocdoni.org": {"a@vocdoni.org", "b@vocdoni.org"},
	} {
		if got := splitList(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitList(%q) = %#v, want %#v", in, got, want)
		}
	}
}
