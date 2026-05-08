// Package agent manages long-lived claude CLI subprocesses, one per
// conversation. Each session wraps a subprocess and an optional MCP server for
// client-defined tools.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/mcpbridge"
)

// ClaudeEvent is a single line from claude's stream-json stdout.
type ClaudeEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Event     json.RawMessage `json:"event"` // present when type=="stream_event"

	// result event fields
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	Error json.RawMessage `json:"error"`
}

// SessionConfig holds spawn parameters.
type SessionConfig struct {
	ClaudeBin    string
	BaseURL      string // ANTHROPIC_BASE_URL for the subprocess self-loop
	AuthToken    string // ANTHROPIC_AUTH_TOKEN
	AllowedTools []string
	WorkDir      string // --add-dir
}

// Session wraps one claude subprocess. It has a single event channel
// (events) that is written by pumpStdout for the lifetime of the subprocess.
// HTTP handlers serialise access with TurnMu.
type Session struct {
	UUID    string
	convKey string
	homeDir string
	cfg     SessionConfig

	mu    sync.Mutex // guards proc / alive
	alive bool
	proc  *exec.Cmd
	stdin io.WriteCloser

	// events is the single output channel from pumpStdout. Reads must be
	// serialised via TurnMu so only one HTTP handler is consuming at a time.
	events chan ClaudeEvent

	// TurnMu serialises turns: acquire before calling Send/Resume, release
	// after the turn's result event is consumed.
	TurnMu sync.Mutex

	mcpMu sync.Mutex
	mcp   *mcpbridge.Server

	LastUsed time.Time
}

// sessionHomeDir returns the path used as HOME for the claude subprocess with
// the given session UUID.
func sessionHomeDir(uuid string) string {
	return filepath.Join(os.TempDir(), "claude-proxy-sessions", uuid)
}

// claudeDataExists reports whether the claude CLI has written session data to
// the given home directory. Used to decide whether --resume is safe to pass.
func claudeDataExists(homeDir string) bool {
	claudeDir := filepath.Join(homeDir, ".claude")
	fi, err := os.Stat(claudeDir)
	return err == nil && fi.IsDir()
}

func newSession(uuid, convKey string, cfg SessionConfig) *Session {
	homeDir := sessionHomeDir(uuid)
	return &Session{
		UUID:     uuid,
		convKey:  convKey,
		homeDir:  homeDir,
		cfg:      cfg,
		events:   make(chan ClaudeEvent, 128),
		LastUsed: time.Now(),
	}
}

// Start spawns the claude subprocess. resume=true adds --resume flag.
func (s *Session) Start(ctx context.Context, resume bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(ctx, resume)
}

func (s *Session) startLocked(ctx context.Context, resume bool) error {
	if err := os.MkdirAll(s.homeDir, 0o700); err != nil {
		return fmt.Errorf("create session home: %w", err)
	}

	args := []string{
		"-p",
		"--bare",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}
	if resume {
		// --resume sets both the session to load and the session ID for this run.
		// Don't also pass --session-id; it conflicts with --resume.
		args = append(args, "--resume", s.UUID)
	} else {
		args = append(args, "--session-id", s.UUID)
	}

	allowed := append([]string{}, s.cfg.AllowedTools...)
	if s.mcp != nil {
		allowed = append(allowed, "mcp__client-tools__*")
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	args = append(args, "--disallowedTools", "Bash,Edit,Write,NotebookEdit")
	if s.cfg.WorkDir != "" {
		args = append(args, "--add-dir", s.cfg.WorkDir)
	}
	if s.mcp != nil {
		mcpCfg := fmt.Sprintf(`{"mcpServers":{"client-tools":{"type":"http","url":%q}}}`,
			s.mcp.URL())
		args = append(args, "--mcp-config", mcpCfg)
	}

	cmd := exec.CommandContext(ctx, s.cfg.ClaudeBin, args...)
	cmd.Env = s.buildEnv()
	cmd.Dir = s.homeDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	s.proc = cmd
	s.stdin = stdin
	s.alive = true

	// Drain and close the old channel; replace with a fresh one.
	oldCh := s.events
	s.events = make(chan ClaudeEvent, 128)
	go func() {
		for range oldCh {
		}
	}()

	// Capture the channel before spawning so the Wait goroutine closes the
	// right channel even if Respawn replaces s.events before cmd exits.
	thisCh := s.events
	go s.pumpStdout(stdout)
	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		s.alive = false
		s.mu.Unlock()
		close(thisCh)
	}()

	return nil
}

func (s *Session) buildEnv() []string {
	env := []string{
		"HOME=" + s.homeDir,
		"PATH=" + os.Getenv("PATH"),
		"CLAUDE_CODE_SIMPLE=1",
		"NO_COLOR=1",
		"CLAUDE_PROXY_HOP=1",
	}
	if s.cfg.BaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+s.cfg.BaseURL)
	}
	// claude --bare requires ANTHROPIC_API_KEY to be non-empty even when the
	// downstream proxy does not enforce auth. Use the configured token if set,
	// otherwise fall back to a placeholder (the proxy ignores the value when
	// PROXY_AUTH_TOKEN is empty).
	apiKey := s.cfg.AuthToken
	if apiKey == "" {
		apiKey = "claude-proxy-no-auth"
	}
	env = append(env, "ANTHROPIC_AUTH_TOKEN="+apiKey)
	env = append(env, "ANTHROPIC_API_KEY="+apiKey)
	return env
}

func (s *Session) pumpStdout(stdout io.ReadCloser) {
	defer stdout.Close()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		var ev ClaudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		s.mu.Lock()
		ch := s.events
		s.mu.Unlock()
		if ch != nil {
			ch <- ev
		}
	}
}

// Send writes a JSONL user message to stdin and returns the session's event
// channel. The caller must hold TurnMu and drain events until a result event
// is received (or the channel is closed). msg must not be nil.
func (s *Session) Send(msg []byte) (<-chan ClaudeEvent, error) {
	s.mu.Lock()
	if !s.alive {
		s.mu.Unlock()
		return nil, fmt.Errorf("session not alive")
	}
	stdin := s.stdin
	ch := s.events
	s.mu.Unlock()

	s.LastUsed = time.Now()
	if _, err := stdin.Write(append(msg, '\n')); err != nil {
		return nil, fmt.Errorf("write stdin: %w", err)
	}
	return ch, nil
}

// Events returns the session's event channel without writing to stdin.
// Used for tool-result continuation (agent is already running).
func (s *Session) Events() (<-chan ClaudeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.alive {
		return nil, fmt.Errorf("session not alive")
	}
	s.LastUsed = time.Now()
	return s.events, nil
}

// SetMCP attaches an MCP server. Must be called before Start for a new session.
func (s *Session) SetMCP(srv *mcpbridge.Server) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	s.mcp = srv
}

// MCP returns the current MCP server (may be nil).
func (s *Session) MCP() *mcpbridge.Server {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	return s.mcp
}

// Kill terminates the subprocess and closes the MCP server.
func (s *Session) Kill() {
	s.mu.Lock()
	if s.proc != nil && s.alive {
		_ = s.proc.Process.Kill()
	}
	s.mu.Unlock()
	s.mcpMu.Lock()
	if s.mcp != nil {
		s.mcp.Close()
		s.mcp = nil
	}
	s.mcpMu.Unlock()
}

// IsAlive reports whether the subprocess is still running.
func (s *Session) IsAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}

// Respawn kills any existing subprocess and starts a new one with --resume.
func (s *Session) Respawn(ctx context.Context) error {
	s.mu.Lock()
	if s.proc != nil && s.alive {
		_ = s.proc.Process.Kill()
	}
	s.alive = false
	s.mu.Unlock()
	return s.Start(ctx, true)
}

// BuildUserMessage serialises a user message for claude's stream-json stdin.
func BuildUserMessage(content json.RawMessage) ([]byte, error) {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": json.RawMessage(content),
		},
	}
	return json.Marshal(msg)
}
