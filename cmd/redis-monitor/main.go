// Command redis-monitor is a standalone Redis key browser, metrics dashboard and
// connected-clients view: one binary that serves both the JSON API and the page
// that consumes it.
//
// Everything it needs is a Redis address and a credential. Sessions live in
// memory, the recorded trend in a JSON file, and the UI is compiled in, so there
// is no database, no asset pipeline and nothing to deploy alongside it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"redismonitor/internal/config"
	"redismonitor/internal/httpapi"
	"redismonitor/internal/monitor"
	"redismonitor/internal/redisx"
	"redismonitor/internal/totp"
	"redismonitor/internal/web"
)

// Build metadata, stamped in at link time by the release workflow:
//
//	-ldflags "-X main.version=v1.2.3 -X main.commit=abc1234 -X main.builtAt=..."
//
// A binary that cannot say which build it is turns "did the fix ship?" into
// guesswork, so this is reported by -version and logged at every start.
var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	envFile := flag.String("env", ".env", "path to the .env file; missing is fine when the environment is already set")
	newSecret := flag.Bool("totp-secret", false, "print a fresh authenticator secret and its otpauth:// URI, then exit")
	account := flag.String("totp-account", "redis-monitor", "account label for the generated otpauth:// URI")
	issuer := flag.String("totp-issuer", "Redis Monitor", "issuer label for the generated otpauth:// URI")
	showVersion := flag.Bool("version", false, "print the build version, then exit")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *showVersion {
		fmt.Printf("redis-monitor %s (commit %s, built %s, %s)\n",
			version, commit, builtAt, runtime.Version())

		return
	}

	if *newSecret {
		if err := printSecret(*account, *issuer); err != nil {
			log.Error("cannot generate a secret", "error", err)
			os.Exit(1)
		}

		return
	}

	if err := run(*envFile, log); err != nil {
		log.Error("shutting down", "error", err)
		os.Exit(1)
	}
}

// printSecret helps with first-time setup: enrol the printed URI in an
// authenticator app and put the secret in REDIS_MONITOR_TOTP_SECRET.
func printSecret(account, issuer string) error {
	secret, err := totp.GenerateSecret(20)
	if err != nil {
		return err
	}

	fmt.Println("REDIS_MONITOR_TOTP_SECRET=" + secret)
	fmt.Println(totp.ProvisioningURI(secret, account, issuer))

	return nil
}

func run(envFile string, log *slog.Logger) error {
	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}

	pool := redisx.NewPool(cfg.Redis, cfg.Databases)
	defer func() {
		if err := pool.Close(); err != nil {
			log.Warn("closing redis pool", "error", err)
		}
	}()

	service, err := monitor.New(cfg, pool, log)
	if err != nil {
		return err
	}

	server := httpapi.New(cfg, service, web.Index, log)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Generous: a deep stats sample against a large keyspace legitimately takes
		// a while, and it is bounded by the sample caps rather than by this.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Report reachability at boot, but do not refuse to start: the monitor exists to
	// show that Redis is down, so it has to come up when Redis is down.
	if err := pool.Ping(ctx, cfg.DefaultDatabase()); err != nil {
		log.Warn("redis is not reachable yet", "addr", cfg.Redis.Addr(), "error", err)
	} else {
		log.Info("redis reachable", "addr", cfg.Redis.Addr(), "database", cfg.DefaultDatabase())
	}

	// Only genuinely credential-less runs deserve the warning: a token-free instance
	// with password login configured is a normal setup, not a mistake.
	if cfg.Token == "" && !cfg.CanLogin() {
		log.Warn("running with no credential at all: REDIS_MONITOR_DEV is set, do not do this anywhere real")
	}

	errs := make(chan error, 1)

	go func() {
		log.Info("listening",
			"addr", cfg.HTTPAddr,
			"enabled", cfg.Enabled,
			"version", version,
			"commit", commit,
		)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err

			return
		}

		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("signal received, draining")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		return httpServer.Shutdown(shutdownCtx)
	}
}
