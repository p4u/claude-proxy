package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

// cacheSaltLabel versions the derivation: changing it starts every user on a
// fresh prompt cache, which is the safe direction.
const cacheSaltLabel = "claude-proxy/privatemode/cache-salt/v1"

// withCacheSalt adds a "cache_salt" to a Messages request body that has none.
//
// The Privatemode proxy salts every request without one at random, so no two
// requests share a prompt cache — and Claude Code re-sends its whole history
// each turn, which makes the cache the difference between paying full input
// price for every turn and paying the cached rate (a tenth, for GLM-5.3). One
// salt for all users would let one user's requests hit another's cache, a
// timing side channel the Privatemode docs warn about. The salt is therefore
// per user and per API key: an HMAC keyed by the key, which only this proxy
// and the user's own credential share, so it cannot be derived or guessed by
// another client. A salt the client set itself is left alone.
func withCacheSalt(ctx context.Context, body []byte, apiKey string) []byte {
	if len(body) == 0 || apiKey == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, set := obj["cache_salt"]; set {
		return body
	}
	user := ""
	if id := usertoken.FromContext(ctx); id != nil {
		user = id.UserTokenID
	}
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(cacheSaltLabel))
	mac.Write([]byte{0})
	mac.Write([]byte(user))
	salt, err := json.Marshal(hex.EncodeToString(mac.Sum(nil)))
	if err != nil {
		return body
	}
	obj["cache_salt"] = salt
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
