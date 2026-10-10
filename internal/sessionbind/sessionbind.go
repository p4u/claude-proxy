// Package sessionbind remembers which credential served each Claude Code
// session, so the GET /v1/claudio/session endpoint can tell a client "this tab
// is running on credential X", and which pool binding served it, so POST
// /v1/claudio/session/switch can ask the pool to move that binding.
//
// Claude Code stamps every request with X-Claude-Code-Session-Id. The proxy
// request path calls Registry.Record once the credential for a request is
// chosen; claudioapi calls Registry.Lookup. State is in memory only (a bounded
// LRU, no database), so it is lost on restart and repopulates on the next
// request of each live session.
//
// Entries are scoped by the caller's identity: Lookup under a different owner
// than the one that recorded the entry reports "not found", so one user can
// never learn another user's session or credential.
//
// Record is O(1), takes one short mutex, never allocates on the hit path and
// never returns an error: it must be impossible for it to fail a request.
package sessionbind

import (
	"container/list"
	"net/http"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// Header is the request header Claude Code uses to identify a session.
const Header = "X-Claude-Code-Session-Id"

// DefaultCapacity bounds the registry. An entry is a few hundred bytes, so the
// default costs a megabyte or two at most.
const DefaultCapacity = 4096

// maxIDLen is the longest session id accepted from a client.
const maxIDLen = 64

// minIDLen rejects obviously degenerate ids ("1", "a") that would collide.
const minIDLen = 8

// Binding is a snapshot of one session's credential binding.
type Binding struct {
	CredentialID string
	// BoundAt is when the session was first seen.
	BoundAt time.Time
	// LastSeen is the most recent request carrying this session id.
	LastSeen time.Time
	// SwitchedAt is when the credential last changed for this session. Zero
	// when the session has only ever used one credential.
	SwitchedAt time.Time
	// Route is the pool binding behind the latest recorded request. Zero
	// when recorded without one, in which case the session cannot be
	// switched.
	Route Route
}

// Route identifies a pool binding: the arguments the proxy passed to
// pool.AcquireScoped (derived conversation ID, provider, and for custom hosts
// the model scope and allowed credential IDs). Allowed is shared, not copied;
// callers must not modify it after recording.
type Route struct {
	ConvID   string
	Provider provider.ID
	Scope    string
	Allowed  []string
}

type entry struct {
	key string
	Binding
}

// Registry is a bounded, concurrency-safe LRU of session bindings. The zero
// value is not usable; construct with New.
type Registry struct {
	mu    sync.Mutex
	cap   int
	order *list.List // front = most recently used; values are *entry
	items map[string]*list.Element
	now   func() time.Time
}

// New returns a Registry holding at most capacity entries. A non-positive
// capacity selects DefaultCapacity.
func New(capacity int) *Registry {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Registry{
		cap:   capacity,
		order: list.New(),
		items: make(map[string]*list.Element, capacity),
		now:   time.Now,
	}
}

// ValidID reports whether s looks like a Claude Code session id: 8 to 64
// characters of [0-9A-Za-z_-] (a UUID fits). Anything else is ignored rather
// than stored, so a hostile header cannot bloat memory or smuggle odd bytes
// into JSON responses.
func ValidID(s string) bool {
	if len(s) < minIDLen || len(s) > maxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// OwnerKey derives the scoping key from the authenticated identity. It matches
// the way the rest of the claudio API distinguishes callers: one key per user
// token, one for the admin token, one for unauthenticated deployments.
func OwnerKey(id *usertoken.Identity) string {
	switch {
	case id == nil:
		return "anon"
	case id.IsAdmin:
		return "admin"
	case id.UserTokenID != "":
		return "user:" + id.UserTokenID
	}
	return "anon"
}

func compose(owner, sessionID string) string { return owner + "\x00" + sessionID }

// Record notes that sessionID (owned by owner) was just served by credID,
// without a pool route. Invalid ids and empty credential ids are ignored. Safe
// on a nil Registry.
func (r *Registry) Record(owner, sessionID, credID string) {
	r.RecordRoute(owner, sessionID, credID, Route{})
}

// RecordRoute is Record that also remembers the pool binding (route) that
// served the request, replacing any earlier route for the session.
func (r *Registry) RecordRoute(owner, sessionID, credID string, route Route) {
	if r == nil || credID == "" || !ValidID(sessionID) {
		return
	}
	k := compose(owner, sessionID)
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.items[k]; ok {
		e := el.Value.(*entry)
		if e.CredentialID != credID {
			e.CredentialID = credID
			e.SwitchedAt = now
		}
		e.LastSeen = now
		e.Route = route
		r.order.MoveToFront(el)
		return
	}
	r.items[k] = r.order.PushFront(&entry{
		key:     k,
		Binding: Binding{CredentialID: credID, BoundAt: now, LastSeen: now, Route: route},
	})
	for r.order.Len() > r.cap {
		oldest := r.order.Back()
		r.order.Remove(oldest)
		delete(r.items, oldest.Value.(*entry).key)
	}
}

// RecordRequest is the proxy-path convenience: it reads the session header and
// the caller identity from req and records the binding and its pool route.
// Requests without a valid header are ignored.
func (r *Registry) RecordRequest(req *http.Request, credID string, route Route) {
	if r == nil {
		return
	}
	sid := req.Header.Get(Header)
	if sid == "" {
		return
	}
	r.RecordRoute(OwnerKey(usertoken.FromContext(req.Context())), sid, credID, route)
}

// Lookup returns the binding for sessionID as recorded under owner. A session
// recorded under a different owner is indistinguishable from an unknown one.
// Lookup does not refresh recency: polling a session must not keep it alive.
func (r *Registry) Lookup(owner, sessionID string) (Binding, bool) {
	if r == nil || !ValidID(sessionID) {
		return Binding{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.items[compose(owner, sessionID)]
	if !ok {
		return Binding{}, false
	}
	return el.Value.(*entry).Binding, true
}

// Len returns the number of tracked sessions.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
}
