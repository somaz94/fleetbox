package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/somaz94/fleetbox/internal/api"
	"github.com/somaz94/fleetbox/internal/auth"
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/console"
	"github.com/somaz94/fleetbox/internal/core"
	"github.com/somaz94/fleetbox/internal/findings"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/notify"
	"github.com/somaz94/fleetbox/internal/store"
	"github.com/somaz94/fleetbox/web"
)

const shutdownTimeout = 10 * time.Second

func newServeCmd() *cobra.Command {
	var configPath, logLevel string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the poller and the HTTP server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", cfg.Server.Listen)
			if err != nil {
				return err
			}
			return serve(cmd.Context(), cfg, ln, logLevel)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug, info, warn or error")
	return cmd
}

// serve runs until ctx ends. The listener is passed in so tests can bind an
// ephemeral port.
func serve(ctx context.Context, cfg *config.Config, ln net.Listener, logLevel string) error {
	defer ln.Close()
	log, err := newLogger(logLevel)
	if err != nil {
		return err
	}
	targets, err := buildTargets(cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.Store.Path)
	if err != nil {
		return err
	}
	defer st.Close()
	authn, err := auth.New(cfg, st, log)
	if err != nil {
		return err
	}
	notifier, err := notify.New(cfg.Notify, cfg.Server.PublicURL, st, log)
	if err != nil {
		return err
	}

	cache := inventory.NewCache()
	poller := inventory.NewPoller(targets, cache, st, log)
	// Background loops get their own context so a server failure stops them too.
	bctx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	pollDone := make(chan struct{})
	go func() {
		poller.Run(bctx)
		close(pollDone)
	}()
	evalDone := make(chan struct{})
	go func() {
		findings.New(cfg.Findings, cache, st, log).Run(bctx)
		close(evalDone)
	}()
	notifyDone := make(chan struct{})
	go func() {
		notifier.Run(bctx)
		close(notifyDone)
	}()

	c := core.New(targets, cache, poller, st, core.BuildInfo{Version: Version, Commit: GitCommit, Date: BuildDate})
	srv := &http.Server{Handler: newMux(api.NewHandler(c, authn, st, log), authn.Routes(), console.Handler(web.Dist())), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("serving", "addr", ln.Addr().String(), "targets", len(targets), "auth", authn.Mode(), "version", Version)

	select {
	case err = <-errc:
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err = srv.Shutdown(sctx)
	}
	stopBackground()
	<-pollDone
	<-evalDone
	<-notifyDone
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// newMux serves the probes, the API, the console and, in oidc mode, the login
// routes. Readiness means configuration loaded and the store opened, which is
// true once the server is listening; it deliberately does not depend on any
// target or the identity provider being reachable.
func newMux(apiHandler, authHandler, consoleHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", consoleHandler)
	mux.Handle("/api/", apiHandler)
	// Without oidc there are no login routes; a 404 says so, where the
	// console catch-all would answer 200 with its page.
	if authHandler == nil {
		authHandler = http.NotFoundHandler()
	}
	mux.Handle("/auth/", authHandler)
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}
