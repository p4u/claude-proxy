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
