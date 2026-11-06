package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/p4u/claude-proxy/internal/plugins"
	"github.com/p4u/claude-proxy/internal/store"
)

const pluginsUsage = `claude-proxy plugins — plugins served to claudio (local DB only)

  plugins list
  plugins set <name> --version V --url https://… --sha256 HEX [--installed-id name@marketplace]
  plugins enable|disable|delete <name>
  plugins env set <name> KEY=- [KEY=-…]     # values read from stdin, one line each, in order
  plugins env set <name> --env-file PATH     # KEY=VALUE lines
  plugins env set <name> KEY=VALUE           # accepted, but the value is visible in ps
  plugins env unset <name> KEY…
  plugins env list <name>                    # keys only

Every subcommand takes --db PATH (default ./proxy.db). Values are never printed.
`

// runPlugins is `claude-proxy plugins …`. It returns the exit code: 0, 1 for
// a failure, 2 for a usage error.
func runPlugins(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "plugins: missing subcommand\n\n"+pluginsUsage)
		return 2
	}
	ctx := context.Background()
	fs := flag.NewFlagSet("plugins "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
	switch args[0] {
	case "list":
		pos, err := parseInterleaved(fs, args[1:])
		if err != nil || len(pos) != 0 {
			fmt.Fprintln(stderr, "plugins list [--db PATH]")
			return 2
		}
		return withDB(*dbPath, stderr, func(db *store.DB) error { return pluginsList(ctx, db, stdout) })
	case "set":
		version := fs.String("version", "", "bundle version (1-64 printable ASCII)")
		url := fs.String("url", "", "https URL of the bundle zip")
		sha := fs.String("sha256", "", "SHA-256 of the zip, 64 lowercase hex")
		installed := fs.String("installed-id", "", "installed Claude Code plugin that makes the bundle redundant (name@marketplace)")
		pos, err := parseInterleaved(fs, args[1:])
		if err != nil || len(pos) != 1 {
			fmt.Fprintln(stderr, "plugins set <name> --version V --url URL --sha256 HEX [--installed-id ID]")
			return 2
		}
		for flagName, v := range map[string]string{"--version": *version, "--url": *url, "--sha256": *sha} {
			if v == "" {
				fmt.Fprintf(stderr, "%s is required\n", flagName)
				return 2
			}
		}
		meta := plugins.Meta{Version: *version, BundleURL: *url, BundleSHA256: *sha, InstalledID: *installed}
		return withDB(*dbPath, stderr, func(db *store.DB) error {
			if err := plugins.Set(ctx, db, pos[0], meta); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s: set to %s\n", pos[0], meta.Version)
			return nil
		})
	case "enable", "disable", "delete":
		pos, err := parseInterleaved(fs, args[1:])
		if err != nil || len(pos) != 1 {
			fmt.Fprintf(stderr, "plugins %s <name> [--db PATH]\n", args[0])
			return 2
		}
		return withDB(*dbPath, stderr, func(db *store.DB) error {
			var err error
			switch args[0] {
			case "enable":
				err = plugins.Enable(ctx, db, pos[0], true)
			case "disable":
				err = plugins.Enable(ctx, db, pos[0], false)
			default:
				err = plugins.Delete(ctx, db, pos[0])
			}
			if err == nil {
				fmt.Fprintf(stdout, "%s: %sd\n", pos[0], strings.TrimSuffix(args[0], "e"))
			}
			return err
		})
	case "env":
		return pluginsEnv(ctx, fs, dbPath, args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "plugins: unknown subcommand %q\n\n%s", args[0], pluginsUsage)
		return 2
	}
}

func pluginsEnv(ctx context.Context, fs *flag.FlagSet, dbPath *string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "plugins env: missing subcommand (set|unset|list)\n")
		return 2
	}
	switch args[0] {
	case "list":
		pos, err := parseInterleaved(fs, args[1:])
		if err != nil || len(pos) != 1 {
			fmt.Fprintln(stderr, "plugins env list <name> [--db PATH]")
			return 2
		}
		return withDB(*dbPath, stderr, func(db *store.DB) error {
			p, err := plugins.Get(ctx, db, pos[0])
			if err != nil {
				return err
			}
			for _, k := range p.EnvKeys {
				fmt.Fprintln(stdout, k)
			}
			return nil
		})
	case "unset":
		pos, err := parseInterleaved(fs, args[1:])
		if err != nil || len(pos) < 2 {
			fmt.Fprintln(stderr, "plugins env unset <name> KEY… [--db PATH]")
			return 2
		}
		return withDB(*dbPath, stderr, func(db *store.DB) error {
			if err := plugins.UnsetEnv(ctx, db, pos[0], pos[1:]...); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s: unset %s\n", pos[0], strings.Join(pos[1:], ", "))
			return nil
		})
	case "set":
		envFile := fs.String("env-file", "", "file of KEY=VALUE lines")
		pos, err := parseInterleaved(fs, args[1:])
		if err != nil || len(pos) < 1 {
			fmt.Fprintln(stderr, "plugins env set <name> KEY=-… | --env-file PATH | KEY=VALUE [--db PATH]")
			return 2
		}
		env := make(map[string]string)
		if *envFile != "" {
			if err := readEnvFile(*envFile, env); err != nil {
				fmt.Fprintf(stderr, "env-file: %v\n", err)
				return 1
			}
		}
		lines := bufio.NewReader(stdin)
		for _, a := range pos[1:] {
			key, value, ok := strings.Cut(a, "=")
			if !ok {
				fmt.Fprintf(stderr, "expected KEY=VALUE or KEY=-, got %q\n", key)
				return 2
			}
			if value == "-" {
				v, err := readLine(lines)
				if err != nil {
					fmt.Fprintf(stderr, "stdin ended before the value of %s\n", key)
					return 1
				}
				value = v
			}
			env[key] = value
		}
		if len(env) == 0 {
			fmt.Fprintln(stderr, "no variables given")
			return 2
		}
		return withDB(*dbPath, stderr, func(db *store.DB) error {
			if err := plugins.SetEnv(ctx, db, pos[0], env); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s: set %d variable(s)\n", pos[0], len(env))
			return nil
		})
	default:
		fmt.Fprintf(stderr, "plugins env: unknown subcommand %q\n", args[0])
		return 2
	}
}

// parseInterleaved parses flags that may come before, between or after the
// positional arguments, which it returns in order.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// withDB opens the database, runs fn and maps its error to an exit code.
// Errors name keys at most, never values (internal/plugins guarantees it).
func withDB(path string, stderr io.Writer, fn func(*store.DB) error) int {
	db, err := store.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "open db %s: %v\n", path, err)
		return 1
	}
	defer db.Close()
	if err := fn(db); err != nil {
		if errors.Is(err, plugins.ErrNotFound) {
			fmt.Fprintln(stderr, "plugins: not found")
		} else {
			fmt.Fprintf(stderr, "plugins: %v\n", err)
		}
		return 1
	}
	return 0
}

func pluginsList(ctx context.Context, db *store.DB, w io.Writer) error {
	list, err := plugins.List(ctx, db)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "(no plugins)")
		return nil
	}
	fmt.Fprintf(w, "%-24s  %-12s  %-3s  %-32s  %-12s  %-25s  %s\n",
		"NAME", "VERSION", "ON", "INSTALLED_ID", "SHA256", "UPDATED", "ENV KEYS / URL")
	for _, p := range list {
		on, installed := "off", p.InstalledID
		if p.Enabled {
			on = "on"
		}
		if installed == "" {
			installed = "-"
		}
		keys := strings.Join(p.EnvKeys, ",")
		if keys == "" {
			keys = "-"
		}
		fmt.Fprintf(w, "%-24s  %-12s  %-3s  %-32s  %-12s  %-25s  %s\n",
			p.Name, p.Version, on, installed, p.BundleSHA256[:12], p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"), keys)
		fmt.Fprintf(w, "%-24s  %s\n", "", p.BundleURL)
	}
	return nil
}

// readEnvFile adds the KEY=VALUE lines of path to env. Blank lines and lines
// starting with # are skipped; one pair of surrounding quotes is removed.
func readEnvFile(path string, env map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		env[strings.TrimSpace(key)] = value
	}
	return sc.Err()
}

// readLine reads one line of stdin without its line ending; EOF before any
// byte is an error.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", io.ErrUnexpectedEOF
	}
	return strings.TrimRight(line, "\r\n"), nil
}
