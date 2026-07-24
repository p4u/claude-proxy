package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAugmentModels(t *testing.T) {
	raw := []byte(`{"data":[` +
		`{"id":"claude-opus-4-8","display_name":"Claude Opus 4.8","type":"model"},` +
		`{"id":"claude-haiku-4-5-20251001","display_name":"Claude Haiku 4.5","type":"model"},` +
		`{"id":"claude-sonnet-4-6-20260101","display_name":"Claude Sonnet 4.6","type":"model"}` +
		`],"has_more":false,"first_id":"claude-opus-4-8","last_id":"claude-sonnet-4-6-20260101"}`)

	out := augmentModels(raw)
	if out == nil {
		t.Fatal("augmentModels returned nil for valid input")
	}
	var env struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
		} `json:"data"`
		HasMore *bool `json:"has_more"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal augmented: %v", err)
	}
	if env.HasMore == nil {
		t.Error("envelope field has_more dropped")
	}
	ids := map[string]string{}
	for _, m := range env.Data {
		ids[m.ID] = m.DisplayName
		if m.Type != "model" {
			t.Errorf("entry %s lost type field", m.ID)
		}
	}
	if dn, ok := ids["claude-opus-4-8[1m]"]; !ok {
		t.Error("missing claude-opus-4-8[1m] variant")
	} else if dn != "Claude Opus 4.8 (1M context)" {
		t.Errorf("bad display name: %q", dn)
	}
	if _, ok := ids["claude-sonnet-4-6-20260101[1m]"]; !ok {
		t.Error("dated snapshot of 1M-capable model not augmented")
	}
	for id := range ids {
		if strings.HasPrefix(id, "claude-haiku") && strings.HasSuffix(id, "[1m]") {
			t.Errorf("haiku (200K) must not get a [1m] variant: %s", id)
		}
	}
	if len(env.Data) != 5 {
		t.Errorf("want 5 entries (3 + 2 variants), got %d", len(env.Data))
	}

	// The [1m] variant must precede its base model so it is the visible row
	// in Claude Code's collapsed /model picker.
	idx := map[string]int{}
	for i, m := range env.Data {
		idx[m.ID] = i
	}
	if idx["claude-opus-4-8[1m]"] > idx["claude-opus-4-8"] {
		t.Error("[1m] variant must come before its base model")
	}
}

func TestAugmentModelsIdempotentAndMalformed(t *testing.T) {
	// Upstream already lists a [1m] id: no duplicate.
	raw := []byte(`{"data":[{"id":"claude-opus-4-8"},{"id":"claude-opus-4-8[1m]"}]}`)
	out := augmentModels(raw)
	if n := strings.Count(string(out), `"claude-opus-4-8[1m]"`); n != 1 {
		t.Errorf("want exactly one [1m] entry, got %d in %s", n, out)
	}

	for _, bad := range []string{`not json`, `{"data":"nope"}`, `[]`} {
		if augmentModels([]byte(bad)) != nil {
			t.Errorf("augmentModels(%q) should return nil", bad)
		}
	}
}

func TestServeModelsAugmentsAndCaches(t *testing.T) {
	hits := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-fable-5","display_name":"Claude Fable 5"},{"id":"claude-haiku-4-5"}],"has_more":false}`))
	})
	h, _, _, _ := setupProxy(t, upstream)

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000", nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
		}
		return rw.Body.String()
	}

	body := get()
	if !strings.Contains(body, `"claude-fable-5[1m]"`) {
		t.Fatalf("response not augmented: %s", body)
	}
	if strings.Contains(body, `"claude-haiku-4-5[1m]"`) {
		t.Fatalf("haiku wrongly augmented: %s", body)
	}

	// Second request must come from cache: upstream hit count stays 1.
	_ = get()
	if hits != 1 {
		t.Errorf("want 1 upstream hit (cache), got %d", hits)
	}

	// Disabled: plain passthrough, no [1m] entries.
	h2, _, _, _ := setupProxy(t, upstream)
	h2.Augment1M = false
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rw := httptest.NewRecorder()
	h2.ServeHTTP(rw, req)
	if strings.Contains(rw.Body.String(), "[1m]") {
		t.Errorf("augmentation ran while disabled: %s", rw.Body.String())
	}
}
