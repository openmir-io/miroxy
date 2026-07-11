package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"miroxy/internal/config"
	"miroxy/internal/server"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the miroxy proxy server",
	Long: `Start the miroxy proxy server.

The server listens on the proxy port for LLM client traffic and on the
admin port for management commands (miroxy stat, health, reload).

Config is read from the path given by -c / --config. Env vars referenced
as ${VAR} in the config file are expanded at load time.`,
	Example: `  miroxy serve
  miroxy serve -c /etc/miroxy/config.yaml
  miroxy serve --port 9090 --admin-port 9091`,
	RunE: runServe,
}

var (
	flagConfig      string
	flagPort        int
	flagAdminPort   int
	flagAdminPass   string
	flagDirect      bool
	flagDump        bool
	flagDumpPath    string
	flagTransparent bool
	flagUpstream    string
)

func init() {
	serveCmd.Flags().StringVarP(&flagConfig, "config", "c", "config/config.yaml", "path to config file")
	serveCmd.Flags().IntVarP(&flagPort, "port", "p", 0, "proxy listen port (overrides config, default 8080)")
	serveCmd.Flags().IntVar(&flagAdminPort, "admin-port", 0, "admin listen port (overrides config, default 8090)")
	serveCmd.Flags().StringVar(&flagAdminPass, "admin-pass", "", "admin API password (default: !miroxy)")
	serveCmd.Flags().BoolVar(&flagDirect, "direct", false, "direct mode: bypass routing, forward to upstream as-is")
	serveCmd.Flags().BoolVar(&flagDump, "dump", false, "dump all request/response pairs to JSONL (overrides config)")
	serveCmd.Flags().StringVar(&flagDumpPath, "dump-path", "", "JSONL dump file path (default: dump.jsonl)")
	serveCmd.Flags().BoolVar(&flagTransparent, "transparent", false, "transparent proxy: forward raw requests, no translation (overrides config)")
	serveCmd.Flags().StringVar(&flagUpstream, "upstream", "", "upstream API base URL for transparent mode (e.g. https://api.anthropic.com)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	if _, statErr := os.Stat(flagConfig); os.IsNotExist(statErr) {
		return fmt.Errorf("config file not found: %s\n\nCreate one from the example:\n  cp config/config.yaml.example config/config.yaml", flagConfig)
	}

	cfg, err := config.NewYAMLStore(flagConfig).Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	configureLogger(cfg.Log.Level, cfg.Log.File)

	// CLI flags override config values.
	if flagPort != 0 {
		cfg.Server.Port = flagPort
	}
	if flagDump {
		cfg.Dump.Enabled = true
		if flagDumpPath != "" {
			cfg.Dump.Path = flagDumpPath
		} else if cfg.Dump.Path == "" {
			cfg.Dump.Path = "dump.jsonl"
		}
	}
	if flagTransparent {
		cfg.Transparent.Enabled = true
		if flagUpstream != "" {
			cfg.Transparent.Upstream = flagUpstream
		}
	}
	// --admin-pass overrides config; empty flag defers to config (which defaults to "!miroxy" in newAdminGuard).
	if flagAdminPass != "" {
		cfg.Admin.Password = flagAdminPass
	}

	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	addr := fmt.Sprintf(":%d", port)

	if flagDirect {
		return runDirect(cfg, addr, flagDumpPath)
	}
	return runProxy(cfg, addr, flagConfig)
}

func runProxy(cfg *config.Config, addr, cfgPath string) error {
	srv := server.New(cfg, cfgPath)

	// Admin server — stays alive for the entire process lifetime.
	var adminSrv *http.Server
	var adminAddr string
	if server.AdminEnabled(cfg) {
		adminAddr = cfg.Admin.Addr
		if adminAddr == "" {
			adminAddr = "127.0.0.1:8090"
		}
		if flagAdminPort != 0 {
			adminAddr = fmt.Sprintf("127.0.0.1:%d", flagAdminPort)
		}
		adminSrv = &http.Server{
			Addr:    adminAddr,
			Handler: srv.AdminHandler(),
		}
		go func() {
			slog.Info("miroxy admin listening", "addr", adminAddr)
			if err := adminSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server error", "error", err)
			}
		}()
	} else {
		slog.Info("miroxy admin server disabled (admin.enabled: false)")
	}

	quit := make(chan os.Signal, 1)
	reload := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(reload, syscall.SIGHUP)

	shutdownAdmin := func() {
		if adminSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = adminSrv.Shutdown(ctx)
			cancel()
		}
	}

	// Proxy start/stop loop.
	// The admin server keeps running across stop/start cycles.
	for {
		httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
		srv.SetProxyRunning(true)
		slog.Info("miroxy proxy starting",
			"addr", addr, "models", len(cfg.ModelRoutes), "log_level", cfg.Log.Level)

		startErr := make(chan error, 1)
		go func() {
			if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				startErr <- err
			}
		}()

	proxyLoop:
		for {
			select {
			case err := <-startErr:
				srv.SetProxyRunning(false)
				shutdownAdmin()
				return fmt.Errorf("proxy: %w", err)

			case <-srv.StopProxyCh():
				srv.SetProxyRunning(false)
				gracefulShutdown(httpSrv, srv)
				slog.Info("miroxy proxy stopped — admin UI still available", "admin", adminAddr)
				break proxyLoop

			case <-reload:
				slog.Info("SIGHUP — reloading config", "path", cfgPath)
				result, err := srv.Reload()
				if err != nil {
					slog.Error("reload failed", "error", err)
				} else {
					slog.Info("reload complete", "changes", result.String())
				}

			case sig := <-quit:
				srv.SetProxyRunning(false)
				slog.Info("shutdown signal", "signal", sig.String(),
					"in_flight", srv.InFlightCount())
				shutdownAdmin()
				gracefulShutdown(httpSrv, srv)
				return nil
			}
		}

		// Paused: wait for restart signal or quit.
	pausedLoop:
		for {
			select {
			case <-srv.StartProxyCh():
				slog.Info("miroxy proxy restarting", "addr", addr)
				cfg = srv.CurrentConfig()
				break pausedLoop

			case <-reload:
				slog.Info("SIGHUP (paused) — reloading config", "path", cfgPath)
				result, err := srv.Reload()
				if err != nil {
					slog.Error("reload failed", "error", err)
				} else {
					slog.Info("reload complete", "changes", result.String())
				}

			case sig := <-quit:
				slog.Info("shutdown signal (paused)", "signal", sig.String())
				shutdownAdmin()
				return nil
			}
		}
	}
}

func gracefulShutdown(httpSrv *http.Server, srv *server.Server) {
	const timeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	deadline, _ := ctx.Deadline()
	slog.Info("shutdown: draining in-flight requests", "timeout", timeout)

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n := srv.InFlightCount(); n > 0 {
					slog.Info("shutdown: waiting for in-flight requests",
						"count", n,
						"timeout_remaining", time.Until(deadline).Round(time.Second).String())
				}
			case <-done:
				return
			}
		}
	}()

	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("shutdown: forced close (timeout exceeded)", "error", err)
	} else {
		slog.Info("shutdown: all requests drained cleanly",
			"in_flight", srv.InFlightCount())
	}
	close(done)
	slog.Info("shutdown complete")
}

func runDirect(cfg *config.Config, addr, dumpPath string) error {
	srv, err := server.NewDirect(cfg, dumpPath)
	if err != nil {
		return fmt.Errorf("create direct server: %w", err)
	}

	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	startErr := make(chan error, 1)
	go func() {
		slog.Info("miroxy starting (direct mode)",
			"addr", addr, "dump", dumpPath)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			startErr <- err
		}
	}()

	select {
	case err := <-startErr:
		return fmt.Errorf("server: %w", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		slog.Info("shutdown complete")
		return nil
	}
}
