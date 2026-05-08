package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/p4u/claude-proxy/internal/mcpbridge"
	"github.com/p4u/claude-proxy/internal/store"
)

// Manager owns the pool of running claude subprocesses.
type Manager struct {
	mu          sync.Mutex
	sessions    map[string]*Session // convKey → Session
	db          *store.DB
	cfg         SessionConfig
	idleTTL     time.Duration
	maxSessions int
}

// New creates a Manager. Call Janitor in a goroutine to reap idle sessions.
func New(db *store.DB, cfg SessionConfig, idleTTL time.Duration, maxSessions int) *Manager {
	if idleTTL <= 0 {
		idleTTL = 10 * time.Minute
	}
	if maxSessions <= 0 {
		maxSessions = 32
	}
	return &Manager{
		sessions:    make(map[string]*Session),
		db:          db,
		cfg:         cfg,
		idleTTL:     idleTTL,
		maxSessions: maxSessions,
	}
}

// GetOrCreate returns an existing live session for convKey or creates a new one.
// tools is the current set of client-defined tools from the request (may be nil).
// Returns the session with its turn lock NOT held; the caller is responsible for
// serialising turns via the session's per-turn channel mechanism.
func (m *Manager) GetOrCreate(ctx context.Context, convKey string, tools []mcpbridge.Tool) (*Session, error) {
	m.mu.Lock()
	sess, ok := m.sessions[convKey]
	if ok && !sess.IsAlive() {
		// Subprocess died; remove so we respawn below.
		delete(m.sessions, convKey)
		ok = false
	}
	m.mu.Unlock()

	if ok {
		// Update MCP tool catalog if needed.
		if mcp := sess.MCP(); mcp != nil {
			mcp.UpdateTools(tools)
		} else if len(tools) > 0 {
			// Tools were added mid-session; need to respawn with MCP support.
			srv, err := mcpbridge.New()
			if err != nil {
				return nil, fmt.Errorf("start mcp server: %w", err)
			}
			srv.UpdateTools(tools)
			sess.Kill()
			sess.SetMCP(srv)
			if err := sess.Start(ctx, true); err != nil {
				return nil, fmt.Errorf("respawn with mcp: %w", err)
			}
		}
		return sess, nil
	}

	// Need a new session. Check if we have a persisted UUID.
	dbSess, err := store.GetAgentSession(ctx, m.db, convKey)
	if err != nil {
		return nil, err
	}

	var sessUUID string
	resume := false
	if dbSess != nil {
		sessUUID = dbSess.SessionUUID
		// Only resume if claude actually wrote session data to the home dir.
		// If the proxy was restarted and /tmp was cleaned, the session data is
		// gone and --resume would fail with "No conversation found".
		homeDir := sessionHomeDir(sessUUID)
		if claudeDataExists(homeDir) {
			resume = true
		} else {
			// Stale DB record; start fresh with a new UUID.
			sessUUID = uuid.New().String()
			_ = store.DeleteAgentSession(ctx, m.db, convKey)
		}
	} else {
		sessUUID = uuid.New().String()
	}

	m.mu.Lock()
	// Double-check after lock (another goroutine may have created it).
	if existing, ok := m.sessions[convKey]; ok && existing.IsAlive() {
		m.mu.Unlock()
		return existing, nil
	}

	// Cap active sessions.
	if len(m.sessions) >= m.maxSessions {
		m.evictOldestLocked()
	}

	sess = newSession(sessUUID, convKey, m.cfg)

	if len(tools) > 0 {
		srv, err := mcpbridge.New()
		if err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("start mcp server: %w", err)
		}
		srv.UpdateTools(tools)
		sess.SetMCP(srv)
	}

	m.sessions[convKey] = sess
	m.mu.Unlock()

	if err := sess.Start(ctx, resume); err != nil {
		m.mu.Lock()
		delete(m.sessions, convKey)
		m.mu.Unlock()
		return nil, fmt.Errorf("start session: %w", err)
	}

	// Persist the UUID so we can resume after proxy restart.
	if err := store.UpsertAgentSession(ctx, m.db, convKey, sessUUID); err != nil {
		// Non-fatal: session still works, just won't survive restarts.
		_ = err
	}

	return sess, nil
}

// Resolve delivers a tool result to the session's MCP server.
func (m *Manager) Resolve(convKey, toolName, content string, isError bool) bool {
	m.mu.Lock()
	sess, ok := m.sessions[convKey]
	m.mu.Unlock()
	if !ok {
		return false
	}
	mcp := sess.MCP()
	if mcp == nil {
		return false
	}
	return mcp.ResolveByName(toolName, mcpbridge.ToolResult{Content: content, IsError: isError})
}

// Kill terminates the session for convKey (if any) and removes it from the pool.
func (m *Manager) Kill(ctx context.Context, convKey string) {
	m.mu.Lock()
	sess, ok := m.sessions[convKey]
	if ok {
		delete(m.sessions, convKey)
	}
	m.mu.Unlock()
	if ok {
		sess.Kill()
	}
	_ = store.DeleteAgentSession(ctx, m.db, convKey)
}

// List returns a snapshot of active sessions for admin display.
func (m *Manager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for key, s := range m.sessions {
		out = append(out, SessionInfo{
			ConvKey:  key,
			UUID:     s.UUID,
			LastUsed: s.LastUsed,
			Alive:    s.IsAlive(),
			HasMCP:   s.MCP() != nil,
		})
	}
	return out
}

// SessionInfo is a read-only view of a session for admin endpoints.
type SessionInfo struct {
	ConvKey  string
	UUID     string
	LastUsed time.Time
	Alive    bool
	HasMCP   bool
}

// Janitor periodically reaps idle sessions. Run in a goroutine.
func (m *Manager) Janitor(ctx context.Context) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	cutoff := time.Now().Add(-m.idleTTL)
	m.mu.Lock()
	var toKill []*Session
	for key, sess := range m.sessions {
		if sess.LastUsed.Before(cutoff) {
			toKill = append(toKill, sess)
			delete(m.sessions, key)
		}
	}
	m.mu.Unlock()
	for _, s := range toKill {
		s.Kill()
	}
}

func (m *Manager) evictOldestLocked() {
	var oldest *Session
	var oldestKey string
	for key, s := range m.sessions {
		if oldest == nil || s.LastUsed.Before(oldest.LastUsed) {
			oldest = s
			oldestKey = key
		}
	}
	if oldest != nil {
		delete(m.sessions, oldestKey)
		go oldest.Kill()
	}
}
