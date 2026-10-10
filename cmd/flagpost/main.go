// Command flagpost runs the flagpost feature-flag server and provides a few
// operational subcommands.
//
//	flagpost serve      run the HTTP server (the default)
//	flagpost migrate    apply database migrations and print their status
//	flagpost bucket     show which rollout bucket a value hashes to
//	flagpost version    print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/api"
	"github.com/J-X0-cloud/flagpost/internal/service"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
	"github.com/J-X0-cloud/flagpost/internal/ui"
	"github.com/J-X0-cloud/flagpost/model"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "flagpost:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		cfg, err := parseServeConfig(args, stderr, getenv)
		if err != nil {
			return err
		}
		return serve(ctx, cfg, stderr, nil)
	case "migrate":
		return migrate(ctx, args, stdout, stderr, getenv)
	case "bucket":
		return bucket(args, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "flagpost", version)
		return nil
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	}
	usage(stderr)
	return fmt.Errorf("unknown command %q", cmd)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: flagpost <command> [flags]

commands:
  serve     run the HTTP server (default)
  migrate   apply database migrations and print their status
  bucket    print the rollout bucket for a seed and value
  version   print the version

Run "flagpost <command> -h" for the flags of a command.
`)
}

type serveConfig struct {
	Addr          string
	DB            string
	AdminToken    string
	LogFormat     string
	LogLevel      slog.Level
	BootstrapEnvs []string
	Heartbeat     time.Duration
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func parseServeConfig(args []string, stderr io.Writer, getenv func(string) string) (serveConfig, error) {
	var cfg serveConfig
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultAddr := ":8080"
	if port := getenv("PORT"); port != "" {
		defaultAddr = ":" + port
	}
	var level, envs string
	fs.StringVar(&cfg.Addr, "addr", envOr(getenv, "FLAGPOST_ADDR", defaultAddr), "listen address (env FLAGPOST_ADDR, or :$PORT)")
	fs.StringVar(&cfg.DB, "db", envOr(getenv, "FLAGPOST_DB", "flagpost.db"), "SQLite database path (env FLAGPOST_DB)")
	fs.StringVar(&cfg.AdminToken, "admin-token", getenv("FLAGPOST_ADMIN_TOKEN"), "admin API token (env FLAGPOST_ADMIN_TOKEN)")
	fs.StringVar(&cfg.LogFormat, "log-format", envOr(getenv, "FLAGPOST_LOG_FORMAT", "text"), "log format: text or json (env FLAGPOST_LOG_FORMAT)")
	fs.StringVar(&level, "log-level", envOr(getenv, "FLAGPOST_LOG_LEVEL", "info"), "log level: debug, info, warn, error (env FLAGPOST_LOG_LEVEL)")
	fs.StringVar(&envs, "bootstrap-envs", getenv("FLAGPOST_BOOTSTRAP_ENVS"), "comma-separated environments to create when the database has none (env FLAGPOST_BOOTSTRAP_ENVS)")
	fs.DurationVar(&cfg.Heartbeat, "heartbeat", 25*time.Second, "interval between keep-alive comments on SDK streams")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if len(cfg.AdminToken) < 16 {
		return cfg, errors.New("an admin token of at least 16 characters is required (set FLAGPOST_ADMIN_TOKEN or -admin-token)")
	}
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return cfg, fmt.Errorf("unknown log format %q", cfg.LogFormat)
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
		return cfg, fmt.Errorf("unknown log level %q", level)
	}
	for _, e := range strings.Split(envs, ",") {
		if e = strings.TrimSpace(e); e != "" {
			if !model.ValidKey(e) {
				return cfg, fmt.Errorf("invalid bootstrap environment key %q", e)
			}
			cfg.BootstrapEnvs = append(cfg.BootstrapEnvs, e)
		}
	}
	return cfg, nil
}

func newLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// serve runs the server until ctx is cancelled. ready, if non-nil, is
// called with the bound address once the listener is open.
func serve(ctx context.Context, cfg serveConfig, logOut io.Writer, ready func(addr string)) error {
	log := newLogger(logOut, cfg.LogFormat, cfg.LogLevel)
	st, err := store.Open(ctx, cfg.DB, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()

	broker := stream.NewBroker(stream.Options{})
	svc := service.New(st, broker, log)
	if err := bootstrap(ctx, svc, cfg.BootstrapEnvs, log); err != nil {
		return err
	}

	srv := api.New(svc, api.Config{AdminToken: cfg.AdminToken, Logger: log, Heartbeat: cfg.Heartbeat})
	admin, err := ui.New(svc, ui.Config{CheckToken: srv.AdminTokenValid, Secret: cfg.AdminToken, Logger: log})
	if err != nil {
		return err
	}
	srv.Handle("/ui/", admin)
	srv.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streaming responses clear their own write deadline.
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	log.Info("flagpost listening", "addr", ln.Addr().String(), "db", cfg.DB, "version", version)
	if ready != nil {
		ready(ln.Addr().String())
	}

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	// Close SDK streams first: http.Server.Shutdown waits for active
	// handlers, and stream handlers only return when their subscription
	// ends. Clients reconnect to another instance or after the restart.
	broker.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// bootstrap creates the given environments when the database has none, so
// a fresh deployment is usable without an extra API call.
func bootstrap(ctx context.Context, svc *service.Service, envs []string, log *slog.Logger) error {
	if len(envs) == 0 {
		return nil
	}
	existing, err := svc.ListEnvironments(ctx)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	ctx = service.WithActor(ctx, "bootstrap")
	for _, key := range envs {
		env, err := svc.CreateEnvironment(ctx, model.Environment{Key: key, Name: titleCase(key)})
		if err != nil {
			return fmt.Errorf("bootstrap environment %q: %w", key, err)
		}
		log.Info("created environment", "key", env.Key)
	}
	return nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func migrate(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", envOr(getenv, "FLAGPOST_DB", "flagpost.db"), "SQLite database path (env FLAGPOST_DB)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(ctx, *db, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := store.AppliedMigrations(ctx, st.DB())
	if err != nil {
		return err
	}
	for _, m := range applied {
		fmt.Fprintf(stdout, "%04d  %-20s  applied %s  %s\n", m.Version, m.Name, m.AppliedAt.Format(time.RFC3339), m.Checksum[:12])
	}
	return nil
}

func bucket(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bucket", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: flagpost bucket <seed> <value>...")
		fmt.Fprintln(stderr, "The seed is the flag's salt, which defaults to its key.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return errors.New("bucket needs a seed and at least one value")
	}
	seed := fs.Arg(0)
	for _, v := range fs.Args()[1:] {
		b := eval.Bucket(seed, v)
		fmt.Fprintf(stdout, "%s\t%d\t%.3f%%\n", v, b, float64(b)/float64(model.TotalWeight)*100)
	}
	return nil
}
