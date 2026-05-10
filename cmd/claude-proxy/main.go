package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"log/slog"

	"github.com/p4u/claude-proxy/internal/admin"
	"github.com/p4u/claude-proxy/internal/agent"
	"github.com/p4u/claude-proxy/internal/bridge"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/ingest"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/prettylog"
	"github.com/p4u/claude-proxy/internal/proxy"
	"github.com/p4u/claude-proxy/internal/store"
)

const usage = `claude-proxy — sticky multi-subscription proxy for Claude Code

Usage:
  claude-proxy serve [flags]
  claude-proxy creds import        --from FILE [--label NAME] [--weight N]
  claude-proxy creds export        [--db PATH]   # JSONL to stdout
  claude-proxy creds import-bulk   [--db PATH]   # JSONL from stdin
  claude-proxy creds list
  claude-proxy creds usage [<id>]
  claude-proxy creds disable <id>
  claude-proxy creds rm <id>
  claude-proxy creds refresh <id>
  claude-proxy creds set-weight <id> <weight>

Run 'claude-proxy <cmd> -h' for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "creds":
		runCreds(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func openDB(path string) *store.DB {
	db, err := store.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db %s: %v\n", path, err)
		os.Exit(1)
	}
	return db
}

func findClaude() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return ""
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8787", "listen address")
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	authToken := fs.String("auth-token", os.Getenv("CLAUDE_PROXY_AUTH_TOKEN"),
		"shared bearer token (env CLAUDE_PROXY_AUTH_TOKEN). empty disables auth.")
	_ = fs.String("on-limited", "passthrough", "behavior when pinned credential is limited")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", "auto", "log format: auto|pretty|text|json")
	logColor := fs.String("log-color", "auto", "log color: auto|always|never")

	// Agent / bridge flags (all overridable via env vars for docker-compose use).
	agentEnableDefault := true
	if v := os.Getenv("ENABLE_API_BRIDGE"); v == "false" || v == "0" {
		agentEnableDefault = false
	}
	enableAgent := fs.Bool("enable-agent", agentEnableDefault,
		"serve /api/v1/* via claude CLI agent (requires claude on PATH; env ENABLE_API_BRIDGE)")

	agentIdleTTLDefault := 10 * time.Minute
	if v := os.Getenv("AGENT_IDLE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			agentIdleTTLDefault = d
		}
	}
	agentIdleTTL := fs.Duration("agent-idle-ttl", agentIdleTTLDefault,
		"kill idle agent sessions after this duration (env AGENT_IDLE_TTL)")

	agentMaxSessionsDefault := 32
	if v := os.Getenv("AGENT_MAX_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			agentMaxSessionsDefault = n
		}
	}
	agentMaxSessions := fs.Int("agent-max-sessions", agentMaxSessionsDefault,
		"max concurrent claude subprocesses (env AGENT_MAX_SESSIONS)")

	agentToolsDefault := "Read,Grep,Glob,WebFetch"
	if v := os.Getenv("AGENT_TOOLS"); v != "" {
		agentToolsDefault = v
	}
	agentTools := fs.String("agent-tools", agentToolsDefault,
		"comma-separated built-in tools allowed for the agent (env AGENT_TOOLS)")

	agentWorkdir := fs.String("agent-workdir", os.Getenv("AGENT_WORKDIR"),
		"directory to mount as --add-dir for the agent (read-only; env AGENT_WORKDIR)")
	agentClaudeBin := fs.String("agent-claude-bin", os.Getenv("AGENT_CLAUDE_BIN"),
		"path to claude binary (default: auto-detect from PATH; env AGENT_CLAUDE_BIN)")
	agentBaseURL := fs.String("agent-base-url", os.Getenv("AGENT_CLAUDE_BASE_URL"),
		"ANTHROPIC_BASE_URL for agent self-loop (default: http://<listen-addr>; env AGENT_CLAUDE_BASE_URL)")

	_ = fs.Parse(args)

	db := openDB(*dbPath)
	defer db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- logger setup ---
	var lvl slog.Level
	switch *logLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	hopts := &slog.HandlerOptions{Level: lvl}
	tty := isTerminal(os.Stderr)
	format := *logFormat
	if format == "auto" {
		if tty {
			format = "pretty"
		} else {
			format = "json"
		}
	}
	useColor := false
	switch *logColor {
	case "always":
		useColor = true
	case "never":
		useColor = false
	default:
		useColor = format == "pretty"
	}
	var handler slog.Handler
	switch format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, hopts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, hopts)
	default:
		handler = prettylog.New(os.Stderr, &prettylog.Options{Level: lvl, Color: useColor})
	}
	logger := slog.New(handler)

	r := creds.NewRefresher(db)
	go r.Loop(ctx)

	p := pool.New(db)
	go p.Janitor(ctx)

	proxyH := proxy.New(db, p, r, logger)
	adminH := admin.New(db)

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxyH)
	mux.Handle("/admin/", adminH)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	// --- agent bridge ---
	if *enableAgent {
		claudeBin := *agentClaudeBin
		if claudeBin == "" {
			claudeBin = findClaude()
		}
		if claudeBin == "" {
			logger.Warn("claude binary not found on PATH; /api endpoints will return 503. " +
				"Install @anthropic-ai/claude-code or set --agent-claude-bin.")
			mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w,
					`{"type":"error","error":{"type":"api_error","message":"claude binary not found; agent disabled"}}`,
					http.StatusServiceUnavailable)
			})
		} else {
			baseURL := *agentBaseURL
			if baseURL == "" {
				// Self-loop: use the proxy's own listen address.
				listenAddr := *addr
				if strings.HasPrefix(listenAddr, ":") {
					listenAddr = "127.0.0.1" + listenAddr
				}
				baseURL = "http://" + listenAddr
			}

			allowedTools := strings.Split(*agentTools, ",")
			for i, t := range allowedTools {
				allowedTools[i] = strings.TrimSpace(t)
			}

			agentCfg := agent.SessionConfig{
				ClaudeBin:    claudeBin,
				BaseURL:      baseURL,
				AuthToken:    *authToken,
				AllowedTools: allowedTools,
				WorkDir:      *agentWorkdir,
			}
			mgr := agent.New(db, agentCfg, *agentIdleTTL, *agentMaxSessions)
			go mgr.Janitor(ctx)

			bridgeH := bridge.New(mgr, proxyH, db, logger)
			mux.Handle("/api/", bridgeH)

			logger.Info("agent bridge enabled",
				"claude", claudeBin,
				"base_url", baseURL,
				"tools", *agentTools,
				"idle_ttl", agentIdleTTL.String(),
				"max_sessions", *agentMaxSessions)
		}
	}

	if *authToken != "" {
		logger.Info("downstream auth enabled (Authorization: Bearer / x-api-key)")
	} else {
		logger.Warn("downstream auth disabled — anyone reaching this proxy can use your credentials")
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: proxy.AuthMiddleware(*authToken, mux),
	}
	go func() {
		<-ctx.Done()
		shutdown, c2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer c2()
		_ = srv.Shutdown(shutdown)
	}()

	logger.Info("serving", "addr", *addr, "db", *dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}

func runCreds(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "creds: missing subcommand (import|export|import-bulk|list|usage|disable|rm|refresh|set-weight)")
		os.Exit(2)
	}
	ctx := context.Background()
	switch args[0] {
	case "import":
		credsImport(ctx, args[1:])
	case "export":
		credsExport(ctx, args[1:])
	case "import-bulk":
		credsImportBulk(ctx, args[1:])
	case "list":
		credsList(ctx, args[1:])
	case "usage":
		credsUsage(ctx, args[1:])
	case "disable":
		credsDisable(ctx, args[1:])
	case "rm":
		credsRm(ctx, args[1:])
	case "refresh":
		credsRefresh(ctx, args[1:])
	case "set-weight":
		credsSetWeight(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "creds: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func credsImport(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("creds import", flag.ExitOnError)
	from := fs.String("from", "", "path to .credentials.json (Claude Code subscription OAuth)")
	label := fs.String("label", "", "label for this credential")
	weight := fs.Int("weight", 0, "round-robin weight (>=1; 0 = derive from subscriptionType)")
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	if *from == "" {
		fmt.Fprintln(os.Stderr, "--from is required")
		os.Exit(2)
	}
	db := openDB(*dbPath)
	defer db.Close()
	c, err := ingest.Import(ctx, db, *from, *label, *weight)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("imported %s  label=%q  sub=%s  weight=%d  expires=%s\n",
		c.ID, c.Label, c.SubscriptionType, c.Weight, c.ExpiresAt.Format(time.RFC3339))
}

func credsList(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("creds list", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	db := openDB(*dbPath)
	defer db.Close()
	list, err := creds.List(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if len(list) == 0 {
		fmt.Println("(no credentials)")
		return
	}
	fmt.Printf("%-30s  %-10s  %-6s  %3s  %-9s  %-25s  %5s/%5s/%5s  %s\n",
		"ID", "LABEL", "SUB", "WT", "STATUS", "EXPIRES", "REQ", "OK", "ERR", "LAST_REQ")
	for _, c := range list {
		lastReq := "-"
		if c.LastRequestAt != nil {
			lastReq = c.LastRequestAt.Format(time.RFC3339)
		}
		fmt.Printf("%-30s  %-10s  %-6s  %3d  %-9s  %-25s  %5d/%5d/%5d  %s\n",
			c.ID, c.Label, c.SubscriptionType, c.Weight, string(c.Status),
			c.ExpiresAt.Format(time.RFC3339),
			c.RequestCount, c.SuccessCount, c.ErrorCount, lastReq)
	}
}

func credsDisable(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "creds disable <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("creds disable", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	if err := creds.SetStatus(ctx, db, args[0], creds.StatusDisabled); err != nil {
		fmt.Fprintf(os.Stderr, "disable: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("disabled", args[0])
}

func credsRm(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "creds rm <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("creds rm", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	if err := creds.Delete(ctx, db, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "rm: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("removed", args[0])
}

func credsSetWeight(ctx context.Context, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "creds set-weight <id> <weight>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("creds set-weight", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[2:])
	w, err := strconv.Atoi(args[1])
	if err != nil || w < 1 {
		fmt.Fprintln(os.Stderr, "weight must be a positive integer")
		os.Exit(2)
	}
	db := openDB(*dbPath)
	defer db.Close()
	if err := creds.SetWeight(ctx, db, args[0], w); err != nil {
		fmt.Fprintf(os.Stderr, "set-weight: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("set weight %d on %s\n", w, args[0])
}

func credsRefresh(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "creds refresh <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("creds refresh", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	r := creds.NewRefresher(db)
	c, err := r.Refresh(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "refresh: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("refreshed %s expires=%s\n", c.ID, c.ExpiresAt.Format(time.RFC3339))
}

// exportLine is the JSONL format used by creds export / import-bulk.
// It embeds the claudeAiOauth structure that ingest.Import expects, plus
// label and weight metadata so a round-trip preserves the full configuration.
type exportLine struct {
	Label  string `json:"label"`
	Weight int    `json:"weight"`
	// claudeAiOauth mirrors internal/ingest credFile.ClaudeAiOauth.
	ClaudeAiOauth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"` // milliseconds
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// credsExport dumps all credentials to stdout as JSONL (one JSON object per line).
// Redirect the output to a file to create a backup:
//
//	claude-proxy creds export --db ./proxy.db > backup.jsonl
func credsExport(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("creds export", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	db := openDB(*dbPath)
	defer db.Close()

	list, err := creds.List(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, c := range list {
		var line exportLine
		line.Label = c.Label
		line.Weight = c.Weight
		line.ClaudeAiOauth.AccessToken = c.AccessToken
		line.ClaudeAiOauth.RefreshToken = c.RefreshToken
		line.ClaudeAiOauth.ExpiresAt = c.ExpiresAt.UnixMilli()
		line.ClaudeAiOauth.Scopes = []string{"user:inference", "user:profile"}
		line.ClaudeAiOauth.SubscriptionType = c.SubscriptionType
		if err := enc.Encode(line); err != nil {
			fmt.Fprintf(os.Stderr, "export: encode %s: %v\n", c.ID, err)
			os.Exit(1)
		}
	}
}

// credsImportBulk reads JSONL from stdin (produced by creds export) and
// imports each credential into the database.
//
//	cat backup.jsonl | claude-proxy creds import-bulk --db ./proxy.db
func credsImportBulk(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("creds import-bulk", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	db := openDB(*dbPath)
	defer db.Close()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	n := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var el exportLine
		if err := json.Unmarshal([]byte(line), &el); err != nil {
			fmt.Fprintf(os.Stderr, "import-bulk: parse line %d: %v\n", n+1, err)
			os.Exit(1)
		}
		// Write to a temp file so ingest.Import can read it.
		tmp, err := os.CreateTemp("", "claude-proxy-import-*.json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "import-bulk: temp file: %v\n", err)
			os.Exit(1)
		}
		// Re-encode only the claudeAiOauth wrapper that ingest expects.
		type credFile struct {
			ClaudeAiOauth any `json:"claudeAiOauth"`
		}
		if err := json.NewEncoder(tmp).Encode(credFile{ClaudeAiOauth: el.ClaudeAiOauth}); err != nil {
			tmp.Close()
			_ = os.Remove(tmp.Name())
			fmt.Fprintf(os.Stderr, "import-bulk: write temp: %v\n", err)
			os.Exit(1)
		}
		tmp.Close()

		c, err := ingest.Import(ctx, db, tmp.Name(), el.Label, el.Weight)
		_ = os.Remove(tmp.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "import-bulk: import %q: %v\n", el.Label, err)
			os.Exit(1)
		}
		fmt.Printf("imported %s  label=%q  sub=%s  weight=%d  expires=%s\n",
			c.ID, c.Label, c.SubscriptionType, c.Weight, c.ExpiresAt.Format(time.RFC3339))
		n++
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "import-bulk: read stdin: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("imported %d credential(s)\n", n)
}

// credsUsage fetches the 5-hour and 7-day usage percentages from Anthropic's
// undocumented OAuth usage endpoint for each credential (or a single one).
func credsUsage(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("creds usage", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	db := openDB(*dbPath)
	defer db.Close()

	list, err := creds.List(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}

	// Filter to a single credential if an ID was given as a positional arg.
	if fs.NArg() > 0 {
		id := fs.Arg(0)
		var filtered []*creds.Credential
		for _, c := range list {
			if c.ID == id {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(os.Stderr, "no credential with id %q\n", id)
			os.Exit(1)
		}
		list = filtered
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for _, c := range list {
		u, fetchErr := fetchUsage(ctx, client, c.AccessToken)
		fmt.Printf("%s  %s  [%s]\n", c.ID, c.Label, string(c.Status))
		if fetchErr != nil {
			fmt.Printf("  error: %s\n", fetchErr.Error())
			continue
		}
		printUsageBucket("  5h     ", &u.FiveHour)
		printUsageBucket("  7d     ", &u.SevenDay)
		printUsageBucket("  7d opus", u.SevenDayOpus)
		printUsageBucket("  7d snnt", u.SevenDaySonnet)
		fmt.Println()
	}
}

type usageBucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type usageResponse struct {
	FiveHour       usageBucket  `json:"five_hour"`
	SevenDay       usageBucket  `json:"seven_day"`
	SevenDayOpus   *usageBucket `json:"seven_day_opus"`
	SevenDaySonnet *usageBucket `json:"seven_day_sonnet"`
}

func printUsageBucket(label string, b *usageBucket) {
	if b == nil || b.Utilization == nil {
		return
	}
	resets := ""
	if b.ResetsAt != nil && *b.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, *b.ResetsAt); err == nil {
			resets = "  resets " + t.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	fmt.Printf("%s  %5.1f%%%s\n", label, *b.Utilization, resets)
}

// fetchUsage calls GET https://api.anthropic.com/api/oauth/usage.
func fetchUsage(ctx context.Context, client *http.Client, accessToken string) (*usageResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result usageResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &result, nil
}
