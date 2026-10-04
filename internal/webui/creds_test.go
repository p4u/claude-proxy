package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestAddCustomOpenAICredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"local-model"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"chatcmpl_test","model":"local-model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "webui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db}

	payload := fmt.Sprintf(`{"provider":"custom_openai","base_url":%q,"api_key":"test-token","label":"local","weight":4}`, upstream.URL+"/v1")
	rec := httptest.NewRecorder()
	s.addCustomCred(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/custom", strings.NewReader(payload)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"provider":"custom_openai"`) {
		t.Fatalf("add response = %d %s", rec.Code, rec.Body.String())
	}

	listed := httptest.NewRecorder()
	s.listCreds(listed, httptest.NewRequest(http.MethodGet, "/api/credentials", nil))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"provider":"custom_openai"`) || !strings.Contains(listed.Body.String(), `"weight":4`) {
		t.Fatalf("list response = %d %s", listed.Code, listed.Body.String())
	}
	if strings.Contains(listed.Body.String(), "test-token") {
		t.Fatal("credential list exposed the bearer token")
	}
}

// TestCredSettings covers the POST /credentials/{id}/settings handler:
// rename, weight change, invalid weight, not-found, and gateway protection.
func TestCredSettings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"test-model"}]}`)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"cmpl_x","model":"test-model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db}

	// Add a custom_openai credential so we have a real ID to update.
	payload := fmt.Sprintf(`{"provider":"custom_openai","base_url":%q,"api_key":"sk-test","label":"before","weight":1}`, upstream.URL+"/v1")
	rec := httptest.NewRecorder()
	s.addCustomCred(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/custom", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("add = %d %s", rec.Code, rec.Body.String())
	}

	// Extract the credential ID from the add response.
	body := rec.Body.String()
	idx := strings.Index(body, `"id":"`)
	if idx < 0 {
		t.Fatalf("no id in add response: %s", body)
	}
	end := strings.Index(body[idx+6:], `"`)
	if end < 0 {
		t.Fatalf("malformed id in add response: %s", body)
	}
	credID := body[idx+6 : idx+6+end]

	t.Run("rename and weight", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"after","weight":7}`)), credID, "settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("settings = %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"label":"after"`) {
			t.Errorf("response missing updated label: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"weight":7`) {
			t.Errorf("response missing updated weight: %s", rec.Body.String())
		}
		// Confirm persistence via list.
		lr := httptest.NewRecorder()
		s.listCreds(lr, httptest.NewRequest(http.MethodGet, "/api/credentials", nil))
		if !strings.Contains(lr.Body.String(), `"label":"after"`) {
			t.Errorf("list does not reflect new label: %s", lr.Body.String())
		}
		if !strings.Contains(lr.Body.String(), `"weight":7`) {
			t.Errorf("list does not reflect new weight: %s", lr.Body.String())
		}
	})

	t.Run("weight unchanged", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"after","weight":7}`)), credID, "settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("idempotent settings = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid weight zero", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"x","weight":0}`)), credID, "settings")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for weight=0, got %d", rec.Code)
		}
	})

	t.Run("invalid weight negative", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"x","weight":-5}`)), credID, "settings")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for weight=-5, got %d", rec.Code)
		}
	})

	t.Run("not found", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/cred_no_such/settings",
			strings.NewReader(`{"label":"x","weight":1}`)), "cred_no_such", "settings")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for unknown id, got %d", rec.Code)
		}
	})

	t.Run("gateway protection", func(t *testing.T) {
		// gateway_codex and gateway_gemini must be blocked at the action level.
		for _, gwID := range []string{"gateway_codex", "gateway_gemini"} {
			rec := httptest.NewRecorder()
			s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+gwID+"/settings",
				strings.NewReader(`{"label":"x","weight":1}`)), gwID, "settings")
			if rec.Code != http.StatusConflict {
				t.Errorf("expected 409 for gateway %q, got %d", gwID, rec.Code)
			}
		}
	})

	t.Run("clearing label", func(t *testing.T) {
		// Empty label is valid — clears any previous display name.
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"","weight":3}`)), credID, "settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("clear label = %d %s", rec.Code, rec.Body.String())
		}
		// Response label field should be "" (label is omitempty in credView, so
		// may be absent from the list; weight must reflect the new value).
		lr := httptest.NewRecorder()
		s.listCreds(lr, httptest.NewRequest(http.MethodGet, "/api/credentials", nil))
		if lr.Code != http.StatusOK {
			t.Fatalf("list after clear = %d %s", lr.Code, lr.Body.String())
		}
		if !strings.Contains(lr.Body.String(), `"weight":3`) {
			t.Errorf("weight not updated after label clear: %s", lr.Body.String())
		}
	})

	t.Run("missing weight defaults to zero → 400", func(t *testing.T) {
		// weight field absent in JSON → decoded as 0 → rejected as < 1.
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"x"}`)), credID, "settings")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing weight, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("label too long → 400, credential unchanged", func(t *testing.T) {
		longLabel := strings.Repeat("x", 201) // 201 Unicode chars > 200 limit
		payload := fmt.Sprintf(`{"label":%q,"weight":3}`, longLabel)
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(payload)), credID, "settings")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for label > 200 chars, got %d: %s", rec.Code, rec.Body.String())
		}
		// The credential weight must not have been changed.
		lr := httptest.NewRecorder()
		s.listCreds(lr, httptest.NewRequest(http.MethodGet, "/api/credentials", nil))
		if strings.Contains(lr.Body.String(), longLabel) {
			t.Error("too-long label was stored despite rejection")
		}
	})

	t.Run("malformed JSON → 400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"x","weight":}`)), credID, "settings")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed JSON, got %d", rec.Code)
		}
	})

	t.Run("provider and endpoint unchanged after rename", func(t *testing.T) {
		// A settings update must only touch label and weight; provider, endpoint,
		// and any stored tokens must remain exactly as they were.
		rec := httptest.NewRecorder()
		s.credAction(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/"+credID+"/settings",
			strings.NewReader(`{"label":"renamed","weight":2}`)), credID, "settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("rename = %d %s", rec.Code, rec.Body.String())
		}
		lr := httptest.NewRecorder()
		s.listCreds(lr, httptest.NewRequest(http.MethodGet, "/api/credentials", nil))
		listed := lr.Body.String()
		if !strings.Contains(listed, `"provider":"custom_openai"`) {
			t.Errorf("provider changed after rename: %s", listed)
		}
		if !strings.Contains(listed, upstream.URL) {
			t.Errorf("endpoint changed after rename: %s", listed)
		}
		// Tokens must never appear in the list response.
		if strings.Contains(listed, "sk-test") {
			t.Error("credential list exposed the API key after rename")
		}
	})
}
