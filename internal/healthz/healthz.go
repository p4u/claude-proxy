package healthz

import (
	"encoding/json"
	"net/http"

	"github.com/p4u/claude-proxy/internal/version"
)

// Handler responds to liveness probes with {"status":"ok","version":"<build>"}.
func Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version.Version,
	})
}
