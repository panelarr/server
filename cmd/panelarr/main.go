// SPDX-License-Identifier: GPL-3.0-or-later

// Command panelarr runs the Panelarr server.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/panelarr/server/internal/api"
	"github.com/panelarr/server/internal/auth"
	"github.com/panelarr/server/internal/buildinfo"
	"github.com/panelarr/server/internal/chapters"
	"github.com/panelarr/server/internal/config"
	"github.com/panelarr/server/internal/downloads"
	"github.com/panelarr/server/internal/events"
	"github.com/panelarr/server/internal/imagecache"
	"github.com/panelarr/server/internal/importer"
	"github.com/panelarr/server/internal/jobs"
	"github.com/panelarr/server/internal/library"
	"github.com/panelarr/server/internal/limits"
	"github.com/panelarr/server/internal/logging"
	"github.com/panelarr/server/internal/meta"
	"github.com/panelarr/server/internal/metrics"
	"github.com/panelarr/server/internal/network"
	"github.com/panelarr/server/internal/plugins"
	"github.com/panelarr/server/internal/queue"
	"github.com/panelarr/server/internal/reader"
	"github.com/panelarr/server/internal/secret"
	"github.com/panelarr/server/internal/storage"
	"github.com/panelarr/server/internal/store"
	"github.com/panelarr/server/internal/users"
	"github.com/panelarr/server/internal/webui"
)

const usage = `panelarr: the Panelarr server

Usage:
  panelarr serve                     run the server (migrates the database first)
  panelarr migrate [up|down|status]  apply, roll back or list database migrations
  panelarr healthcheck [--url URL]   probe a running server (for container health checks)
  panelarr version                   print the version

Settings come from PANELARR_* environment variables or flags; run
"panelarr serve -h" for the flags. See docs/configuration.md.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	if errors.Is(err, errUsage) {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "panelarr:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "version":
		_, err := fmt.Fprintln(stdout, buildinfo.Version)
		return err
	case "serve":
		return serve(ctx, args[1:], stdout, stderr, getenv, nil)
	case "migrate":
		return migrate(ctx, args[1:], stdout, stderr, getenv)
	case "healthcheck":
		if code := runHealthcheck(args[1:], stderr, getenv); code != 0 {
			return fmt.Errorf("unhealthy (exit %d)", code)
		}
		return nil
	default:
		return errUsage
	}
}

// loadConfig parses the flags in args and loads the configuration.
func loadConfig(name string, args []string, stderr io.Writer, getenv func(string) string) (config.Config, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f config.Flags
	f.Register(fs)
	if err := fs.Parse(args); err != nil {
		return config.Config{}, nil, err
	}
	cfg, err := f.Load(getenv)
	return cfg, fs.Args(), err
}

// serve runs the server until ctx is cancelled. ready, when not nil,
// receives the listeners' addresses (API first) once they listen (tests).
func serve(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, ready chan<- []string) error {
	cfg, rest, err := loadConfig("serve", args, stderr, getenv)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errUsage
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	log := logging.New(cfg.Log, stdout)
	// Background work stops with ctx, or when serving fails.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log.Info("starting", "version", buildinfo.Version, "listen", cfg.Listen, "config_dir", cfg.ConfigDir, "data_dir", cfg.DataDir)

	for _, dir := range []string{cfg.ConfigDir, cfg.DataDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := st.Migrate(ctx)
	if err != nil {
		return err
	}
	for _, m := range applied {
		log.Info("migration applied", "component", "store", "version", m.Version, "name", m.Name)
	}

	var m *metrics.Metrics
	if cfg.Metrics.Enabled {
		m = metrics.New()
	}
	authSvc := auth.NewService(st, auth.NewHasher(auth.DefaultArgon2, 4), logging.Component(log, "auth"))
	usersSvc := users.NewService(st, authSvc, logging.Component(log, "users"))
	hub := events.NewHub()
	lim := limits.NewRegistry()

	box := secret.NewBox(cfg.SecretKey, filepath.Join(cfg.ConfigDir, "secret.key"))
	metaSvc := meta.NewService(st, box, meta.Conn{URL: cfg.MetaURL, APIKey: cfg.MetaAPIKey}, logging.Component(log, "meta"))
	if err := metaSvc.Load(ctx); err != nil {
		// The server still starts: degraded, and an admin can reconnect.
		log.Error("metadata service connection", "component", "meta", "err", err)
	}
	images := imagecache.New(filepath.Join(cfg.ConfigDir, "cache", "images"))
	signer, err := imagecache.LoadSigner(ctx, st)
	if err != nil {
		return err
	}
	host := plugins.NewHost(st, box, plugins.RemoteDialer, logging.Component(log, "plugins"))
	host.SetLimits(lim)
	host.SetHub(hub)
	host.SetMetrics(m)
	q := queue.New(st, hub, lim, m, logging.Component(log, "queue"))
	lib := library.NewService(st, metaSvc, host, q, lim, hub, logging.Component(log, "library"))
	chs := chapters.NewService(st, host, q, lim, hub, logging.Component(log, "chapters"))
	lib.SetChapterHooks(chs)
	host.SetSourcesChanged(chs.SourcesChanged)
	host.RegisterPackageTasks(q)
	netSvc := network.New(st, box, logging.Component(log, "network"))
	host.SetNetwork(netSvc, q, func(ctx context.Context) error {
		_, _, err := netSvc.PushMeta(ctx, metaSvc, false)
		return err
	})
	// Connecting to plugins can take a while; the API serves meanwhile.
	go func() {
		if err := host.Load(ctx); err != nil {
			log.Error("load plugins", "component", "plugins", "err", err)
		}
	}()
	rdr := reader.NewService(st, host, lib, chs, lim, hub, logging.Component(log, "reader"))
	storages := storage.NewManager(st, box, logging.Component(log, "storage"))
	if err := storages.EnsureDefault(ctx, filepath.Join(cfg.DataDir, "library")); err != nil {
		return err
	}
	dl := downloads.NewService(st, host, storages, metaSvc, q, lim, hub, filepath.Join(cfg.DataDir, ".tmp"), logging.Component(log, "downloads"))
	rdr.SetFiles(dl)
	chs.OnNewChapters(dl.NewChapters)
	imp := importer.NewService(st, lib, metaSvc, host, q, hub, importer.Options{TempDir: filepath.Join(cfg.DataDir, ".tmp")}, logging.Component(log, "importer"))
	jm := jobs.NewManager(st, hub, m, logging.Component(log, "jobs"))
	registerJobs(jm, st, authSvc, host, lib, chs, netSvc, metaSvc)
	if err := jm.Start(ctx); err != nil {
		return err
	}
	queueDone := make(chan struct{})
	go func() {
		defer close(queueDone)
		if err := q.Run(ctx); err != nil {
			log.Error("task queue", "component", "queue", "err", err)
		}
	}()
	defer func() {
		cancel()
		<-queueDone
		jm.Wait()
	}()

	webFiles, err := webui.Files(cfg.WebDir)
	if err != nil {
		return fmt.Errorf("%s: %w", config.EnvWebDir, err)
	}
	if webui.IsPlaceholder(webFiles) {
		log.Warn("this build has no web app: the API works, the browser gets a placeholder page")
	}

	handler, err := api.NewHandler(api.Deps{
		Log:          logging.Component(log, "api"),
		Metrics:      m,
		MetricsOnAPI: cfg.Metrics.Listen == "",
		Checks: []api.Check{
			{Name: "database", Required: true, Run: st.Pool.Ping},
			// The library needs the metadata service, but reading what's
			// downloaded doesn't: without it the server is degraded.
			{Name: "meta", Run: metaSvc.ReadyCheck},
			{Name: "plugins", Run: host.ReadyCheck},
		},
		Auth:           authSvc,
		Users:          usersSvc,
		Meta:           metaSvc,
		Images:         images,
		Plugins:        host,
		Signer:         signer,
		Jobs:           jm,
		Queue:          q,
		Hub:            hub,
		Store:          st,
		Library:        lib,
		Chapters:       chs,
		Reader:         rdr,
		Downloads:      dl,
		Storages:       storages,
		Network:        netSvc,
		Importer:       imp,
		SecureCookies:  cfg.SecureCookies(),
		TrustedProxies: cfg.TrustedProxies,
		Web:            webui.Handler(webFiles),
	})
	if err != nil {
		return err
	}

	servers := []*namedServer{{name: "api", addr: cfg.Listen, srv: newHTTPServer(handler)}}
	if m != nil && cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		servers = append(servers, &namedServer{name: "metrics", addr: cfg.Metrics.Listen, srv: newHTTPServer(mux)})
	}
	return runServers(ctx, log, servers, ready)
}

type namedServer struct {
	name string
	addr string
	srv  *http.Server
}

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: page streams and the event stream are long-lived.
	}
}

// runServers listens on every address, serves until ctx ends or a server
// fails, then shuts all of them down gracefully.
func runServers(ctx context.Context, log *slog.Logger, servers []*namedServer, ready chan<- []string) error {
	var lc net.ListenConfig
	listeners := make([]net.Listener, 0, len(servers))
	for _, s := range servers {
		ln, err := lc.Listen(ctx, "tcp", s.addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("%s: listen on %s: %w", s.name, s.addr, err)
		}
		listeners = append(listeners, ln)
		log.Info("listening", "server", s.name, "addr", ln.Addr().String())
	}
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() {
			if err := s.srv.Serve(listeners[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", s.name, err)
			}
		}()
	}
	if ready != nil {
		addrs := make([]string, len(listeners))
		for i, l := range listeners {
			addrs[i] = l.Addr().String()
		}
		ready <- addrs
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		if serr := s.srv.Shutdown(shutdownCtx); serr != nil {
			log.Warn("shutdown", "server", s.name, "err", serr)
		}
	}
	return err
}

func migrate(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	cfg, rest, err := loadConfig("migrate", args, stderr, getenv)
	if err != nil {
		return err
	}
	cmd := "up"
	if len(rest) > 1 {
		return errUsage
	}
	if len(rest) == 1 {
		cmd = rest[0]
	}
	if cmd != "up" && cmd != "down" && cmd != "status" {
		return errUsage
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd {
	case "up":
		res, err := st.Migrate(ctx)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			_, err = fmt.Fprintln(stdout, "database is up to date")
			return err
		}
		for _, r := range res {
			if _, err := fmt.Fprintf(stdout, "applied %d %s\n", r.Version, r.Name); err != nil {
				return err
			}
		}
	case "down":
		res, err := st.MigrateDown(ctx)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			_, err = fmt.Fprintln(stdout, "nothing to roll back")
			return err
		}
		for _, r := range res {
			if _, err := fmt.Fprintf(stdout, "rolled back %d %s\n", r.Version, r.Name); err != nil {
				return err
			}
		}
	case "status":
		rows, err := st.Status(ctx)
		if err != nil {
			return err
		}
		for _, m := range rows {
			state := "pending"
			if m.Applied {
				state = "applied"
			}
			if _, err := fmt.Fprintf(stdout, "%-8s %d %s\n", state, m.Version, m.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
