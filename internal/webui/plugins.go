package webui

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/plugins"
)

// pluginView is a plugin on the Plugins page: env keys, never values.
type pluginView struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	BundleURL    string   `json:"bundle_url"`
	BundleSHA256 string   `json:"bundle_sha256"`
	InstalledID  string   `json:"installed_id"`
	Enabled      bool     `json:"enabled"`
	UpdatedAt    string   `json:"updated_at"`
	EnvKeys      []string `json:"env_keys"`
}

func viewOf(p plugins.Plugin) pluginView {
	return pluginView{
		Name: p.Name, Version: p.Version, BundleURL: p.BundleURL, BundleSHA256: p.BundleSHA256,
		InstalledID: p.InstalledID, Enabled: p.Enabled, UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
		EnvKeys: p.EnvKeys,
	}
}

// handlePlugins routes /plugins. `rest` is the path after "/plugins":
//
//	GET    ""                 list
//	PUT    /{name}            create or replace the bundle reference (env untouched)
//	POST   /{name}/enable     | /{name}/disable
//	DELETE /{name}
//	PUT    /{name}/env        upsert the listed keys {"KEY":"value",…}
//	DELETE /{name}/env/{KEY}
//
// No route ever returns an env value.
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request, rest string) {
	ctx := r.Context()
	if rest == "" {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		list, err := plugins.List(ctx, s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]pluginView, 0, len(list))
		for _, p := range list {
			out = append(out, viewOf(p))
		}
		writeJSON(w, map[string]any{"plugins": out})
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	name := parts[0]
	var err error
	switch {
	case len(parts) == 1 && r.Method == http.MethodPut:
		var body struct {
			Version      string `json:"version"`
			BundleURL    string `json:"bundle_url"`
			BundleSHA256 string `json:"bundle_sha256"`
			InstalledID  string `json:"installed_id"`
		}
		if derr := decodeJSON(w, r, &body); derr != nil {
			writeErr(w, http.StatusBadRequest, derr.Error())
			return
		}
		err = plugins.Set(ctx, s.db, name, plugins.Meta{
			Version: body.Version, BundleURL: body.BundleURL,
			BundleSHA256: body.BundleSHA256, InstalledID: body.InstalledID,
		})
	case len(parts) == 1 && r.Method == http.MethodDelete:
		err = plugins.Delete(ctx, s.db, name)
	case len(parts) == 2 && parts[1] == "enable" && r.Method == http.MethodPost:
		err = plugins.Enable(ctx, s.db, name, true)
	case len(parts) == 2 && parts[1] == "disable" && r.Method == http.MethodPost:
		err = plugins.Enable(ctx, s.db, name, false)
	case len(parts) == 2 && parts[1] == "env" && r.Method == http.MethodPut:
		var env map[string]string
		if derr := decodeJSON(w, r, &env); derr != nil {
			// The decoder's message may quote the body; never echo it.
			writeErr(w, http.StatusBadRequest, "body must be a JSON object of string values")
			return
		}
		err = plugins.SetEnv(ctx, s.db, name, env)
	case len(parts) == 3 && parts[1] == "env" && r.Method == http.MethodDelete:
		err = plugins.UnsetEnv(ctx, s.db, name, parts[2])
	default:
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case errors.Is(err, plugins.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, plugins.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		s.writePlugin(w, r, name)
	}
}

// writePlugin answers a successful change with the plugin as it now is, or
// {"ok":true} once it is gone.
func (s *Server) writePlugin(w http.ResponseWriter, r *http.Request, name string) {
	p, err := plugins.Get(r.Context(), s.db, name)
	if errors.Is(err, plugins.ErrNotFound) {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, viewOf(*p))
}
