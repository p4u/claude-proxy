package healthz

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rw := httptest.NewRecorder()

	Handler(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rw.Code)
	}
}

func TestHealthzContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rw := httptest.NewRecorder()

	Handler(rw, req)

	ct := rw.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("expected Content-Type to contain application/json, got %q", ct)
	}
}

func TestHealthzBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rw := httptest.NewRecorder()

	Handler(rw, req)

	var body map[string]string
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v — body was: %s", err, rw.Body.String())
	}

	if body["status"] != "ok" {
		t.Fatalf("expected body[\"status\"] == \"ok\", got %q", body["status"])
	}
	if body["version"] == "" {
		t.Fatalf("expected body[\"version\"] to be non-empty")
	}
}
