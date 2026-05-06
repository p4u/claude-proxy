package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"log/slog"
	"net/http"

	"github.com/p4u/claude-proxy/internal/admin"
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
  claude-proxy creds import --from FILE [--label NAME] [--weight N]
  claude-proxy creds list
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

// isTerminal returns true if f is a character device (tty/pty), false for
// pipes/files. Avoids depending on golang.org/x/term.
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
		"shared bearer token required from clients (env CLAUDE_PROXY_AUTH_TOKEN). empty disables auth.")
	_ = fs.String("on-limited", "passthrough", "behavior when pinned credential is limited")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	logFormat := fs.String("log-format", "auto", "log format: auto|pretty|text|json (auto: pretty on tty, json otherwise)")
	logColor := fs.String("log-color", "auto", "log color: auto|always|never")
	_ = fs.Parse(args)

	db := openDB(*dbPath)
	defer db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

	// Color default:
	//   --log-color=always  -> on
	//   --log-color=never   -> off
	//   --log-color=auto    -> on whenever the active format is "pretty"
	//                          (covers the common docker/ssh case where the
	//                          operator explicitly asked for pretty logs).
	useColor := false
	switch *logColor {
	case "always":
		useColor = true
	case "never":
		useColor = false
	default: // "auto"
		useColor = format == "pretty"
	}
	var handler slog.Handler
	switch format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, hopts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, hopts)
	default: // "pretty"
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

	if *authToken != "" {
		logger.Info("downstream auth enabled (clients must send Authorization: Bearer <token>)")
	} else {
		logger.Warn("downstream auth disabled — anyone reaching this proxy can use your credentials")
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: proxy.AuthMiddleware(*authToken, mux),
		// no read/write timeouts: SSE streams can be very long
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
		fmt.Fprintln(os.Stderr, "creds: missing subcommand (import|list|disable|rm|refresh)")
		os.Exit(2)
	}
	ctx := context.Background()
	switch args[0] {
	case "import":
		credsImport(ctx, args[1:])
	case "list":
		credsList(ctx, args[1:])
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
