package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/p4u/claude-proxy/internal/admin"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/ingest"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/prettylog"
	"github.com/p4u/claude-proxy/internal/proxy"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/tui"
	"github.com/p4u/claude-proxy/internal/usage"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

const helpText = `claude-proxy — sticky multi-subscription proxy for Claude Code

Usage:
  claude-proxy serve [flags]
  claude-proxy users create           --name NAME [--db PATH]
  claude-proxy users list             [--db PATH]
  claude-proxy users stats [<id>]     [--period 1h|6h|24h|7d|30d] [--db PATH]
  claude-proxy users token <id>       [--db PATH]
  claude-proxy users disable <id>     [--db PATH]
  claude-proxy users enable <id>      [--db PATH]
  claude-proxy users rm <id>          [--db PATH]
  claude-proxy users refresh <id>     [--db PATH]
  claude-proxy tui                 [--db PATH]   # interactive management UI
  claude-proxy creds import        --from FILE [--label NAME] [--weight N]
  claude-proxy creds update <id>   --from FILE   # replace tokens from a fresh login
  claude-proxy creds export        [--db PATH]   # JSONL to stdout
  claude-proxy creds import-bulk   [--db PATH]   # JSONL from stdin
  claude-proxy creds list
  claude-proxy creds usage [<id>]
  claude-proxy creds usage-history [--period 1h|6h|24h|7d|30d] [<id>]
  claude-proxy creds disable <id>
  claude-proxy creds rm <id>
  claude-proxy creds refresh <id>
  claude-proxy creds set-weight <id> <weight>

Run 'claude-proxy <cmd> -h' for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "tui":
		runTUI(os.Args[2:])
	case "creds":
		runCreds(os.Args[2:])
	case "users":
		runUsers(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(helpText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], helpText)
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

	p := pool.NewWithLogger(db, logger)
	go p.Janitor(ctx)

	go usage.NewPoller(db, logger).Loop(ctx)

	proxyH := proxy.New(db, p, r, logger)
	adminH := admin.New(db)

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxyH)
	mux.Handle("/admin/", adminH)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	if *authToken != "" {
		logger.Info("downstream auth enabled (Authorization: Bearer / x-api-key)")
	} else {
		logger.Warn("downstream auth disabled — anyone reaching this proxy can use your credentials")
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: proxy.AuthMiddleware(*authToken, db, mux),
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

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	db := openDB(*dbPath)
	defer db.Close()
	r := creds.NewRefresher(db)
	if err := tui.Run(db, r); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
}

func runCreds(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "creds: missing subcommand (import|update|export|import-bulk|list|usage|usage-history|disable|rm|refresh|set-weight)")
		os.Exit(2)
	}
	ctx := context.Background()
	switch args[0] {
	case "import":
		credsImport(ctx, args[1:])
	case "update":
		credsUpdate(ctx, args[1:])
	case "export":
		credsExport(ctx, args[1:])
	case "import-bulk":
		credsImportBulk(ctx, args[1:])
	case "list":
		credsList(ctx, args[1:])
	case "usage":
		credsUsage(ctx, args[1:])
	case "usage-history":
		credsUsageHistory(ctx, args[1:])
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

func credsUpdate(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "creds update <id> --from FILE")
		os.Exit(2)
	}
	id := args[0]
	fs := flag.NewFlagSet("creds update", flag.ExitOnError)
	from := fs.String("from", "", "path to a fresh .credentials.json (Claude Code re-login)")
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	if *from == "" {
		fmt.Fprintln(os.Stderr, "--from is required")
		os.Exit(2)
	}
	db := openDB(*dbPath)
	defer db.Close()
	c, err := ingest.UpdateFromFile(ctx, db, id, *from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("updated %s  label=%q  sub=%s  status=%s  expires=%s\n",
		c.ID, c.Label, c.SubscriptionType, string(c.Status), c.ExpiresAt.Format(time.RFC3339))
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

// credsUsage fetches live usage from Anthropic for each credential (or one).
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
		u, fetchErr := usage.Fetch(ctx, client, c.AccessToken)
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

func printUsageBucket(label string, b *usage.Bucket) {
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

func runUsers(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "users: missing subcommand (create|list|stats|token|disable|enable|rm|refresh)")
		os.Exit(2)
	}
	ctx := context.Background()
	switch args[0] {
	case "create":
		usersCreate(ctx, args[1:])
	case "list":
		usersList(ctx, args[1:])
	case "stats":
		usersStats(ctx, args[1:])
	case "token":
		usersToken(ctx, args[1:])
	case "disable":
		usersDisable(ctx, args[1:])
	case "enable":
		usersEnable(ctx, args[1:])
	case "rm":
		usersRm(ctx, args[1:])
	case "refresh":
		usersRefresh(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "users: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func usersCreate(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("users create", flag.ExitOnError)
	name := fs.String("name", "", "user name (unique label)")
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	if *name == "" {
		fmt.Fprintln(os.Stderr, "--name is required")
		os.Exit(2)
	}
	db := openDB(*dbPath)
	defer db.Close()
	ut, err := usertoken.Create(ctx, db, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created %s  name=%q  token=%s\n", ut.ID, ut.Name, ut.Token)
}

func usersList(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("users list", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args)
	db := openDB(*dbPath)
	defer db.Close()
	list, err := usertoken.List(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if len(list) == 0 {
		fmt.Println("(no user tokens)")
		return
	}
	fmt.Printf("%-22s  %-20s  %-8s  %-25s  %s\n", "ID", "NAME", "STATUS", "CREATED", "LAST_USED")
	for _, ut := range list {
		lastUsed := "-"
		if ut.LastUsedAt != nil {
			lastUsed = ut.LastUsedAt.Format(time.RFC3339)
		}
		fmt.Printf("%-22s  %-20s  %-8s  %-25s  %s\n",
			ut.ID, ut.Name, string(ut.Status),
			ut.CreatedAt.Format(time.RFC3339), lastUsed)
	}
}

func usersStats(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("users stats", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	period := fs.String("period", "24h", "time window: 1h, 6h, 24h, 7d, 30d")
	_ = fs.Parse(args)

	dur, err := usage.ParsePeriod(*period)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	since := time.Now().Add(-dur).Unix()

	db := openDB(*dbPath)
	defer db.Close()

	// Optional: filter to a single user token (positional arg).
	filterID := ""
	if fs.NArg() > 0 {
		filterID = fs.Arg(0)
	}

	list, err := usertoken.List(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}

	type row struct {
		Requests   int64
		OK         int64
		Errors     int64
		BytesSent  int64
		BytesRecv  int64
		LatencySum int64
		Convs      int64
	}

	fmt.Printf("Period: last %s\n\n", *period)
	fmt.Printf("%-22s  %-20s  %8s  %6s  %6s  %10s  %10s  %9s  %6s\n",
		"ID", "NAME", "REQUESTS", "OK", "ERR", "SENT", "RECEIVED", "AVG_LAT", "CONVS")

	printed := 0
	for _, ut := range list {
		if filterID != "" && ut.ID != filterID {
			continue
		}
		var r row
		err := db.QueryRowContext(ctx, `
			SELECT
				COUNT(*),
				SUM(CASE WHEN status_code=200 THEN 1 ELSE 0 END),
				SUM(CASE WHEN status_code>=400 OR status_code=-1 THEN 1 ELSE 0 END),
				COALESCE(SUM(bytes_sent),0),
				COALESCE(SUM(bytes_received),0),
				COALESCE(SUM(latency_ms),0),
				COUNT(DISTINCT CASE WHEN conv_id!='' THEN conv_id END)
			FROM request_log
			WHERE user_token_id=? AND ts>=?`, ut.ID, since).
			Scan(&r.Requests, &r.OK, &r.Errors, &r.BytesSent, &r.BytesRecv, &r.LatencySum, &r.Convs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query %s: %v\n", ut.ID, err)
			continue
		}
		avgLat := "-"
		if r.Requests > 0 {
			avgLat = fmt.Sprintf("%dms", r.LatencySum/r.Requests)
		}
		fmt.Printf("%-22s  %-20s  %8d  %6d  %6d  %10s  %10s  %9s  %6d\n",
			ut.ID, ut.Name,
			r.Requests, r.OK, r.Errors,
			fmtBytes(r.BytesSent), fmtBytes(r.BytesRecv),
			avgLat, r.Convs)
		printed++
	}
	if printed == 0 && filterID != "" {
		fmt.Fprintf(os.Stderr, "no user token with id %q\n", filterID)
		os.Exit(1)
	}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func usersToken(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "users token <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("users token", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	ut, err := usertoken.Get(ctx, db, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "token: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(ut.Token)
}

func usersDisable(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "users disable <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("users disable", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	if err := usertoken.SetStatus(ctx, db, args[0], usertoken.StatusDisabled); err != nil {
		fmt.Fprintf(os.Stderr, "disable: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("disabled", args[0])
}

func usersEnable(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "users enable <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("users enable", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	if err := usertoken.SetStatus(ctx, db, args[0], usertoken.StatusActive); err != nil {
		fmt.Fprintf(os.Stderr, "enable: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("enabled", args[0])
}

func usersRm(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "users rm <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("users rm", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	if err := usertoken.Delete(ctx, db, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "rm: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("removed", args[0])
}

func usersRefresh(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "users refresh <id>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("users refresh", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	_ = fs.Parse(args[1:])
	db := openDB(*dbPath)
	defer db.Close()
	token, err := usertoken.Refresh(ctx, db, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "refresh: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("refreshed %s  token=%s\n", args[0], token)
}

// credsUsageHistory renders terminal charts from stored usage snapshots.
func credsUsageHistory(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("creds usage-history", flag.ExitOnError)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	period := fs.String("period", "24h", "time window: 1h, 6h, 24h, 7d, 30d")
	_ = fs.Parse(args)

	dur, err := usage.ParsePeriod(*period)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	db := openDB(*dbPath)
	defer db.Close()

	since := time.Now().Add(-dur)

	list, err := creds.List(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}

	// Optional: filter to a single credential.
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

	for _, c := range list {
		snaps, err := usage.History(ctx, db, c.ID, since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "history %s: %v\n", c.ID, err)
			continue
		}
		fmt.Printf("%s  %s  [%s]\n", c.ID, c.Label, string(c.Status))
		fmt.Println(usage.Chart(snaps, c.Label, *period))
	}
}
