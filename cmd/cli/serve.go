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
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/core"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/store"
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
	// Refuse rather than accept the setting and serve unauthenticated.
	if cfg.Auth.OIDC != nil {
		return errors.New("auth.oidc is not implemented yet (planned for v0.2): remove it and bind server.listen to 127.0.0.1")
	}
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

	cache := inventory.NewCache()
	poller := inventory.NewPoller(targets, cache, st, log)
	// The poller gets its own context so a server failure stops it too.
	pctx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	pollDone := make(chan struct{})
	go func() {
		poller.Run(pctx)
		close(pollDone)
	}()

	c := core.New(targets, cache, poller, st, core.BuildInfo{Version: Version, Commit: GitCommit, Date: BuildDate})
	srv := &http.Server{Handler: newMux(api.NewHandler(c, log)), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("serving", "addr", ln.Addr().String(), "targets", len(targets), "version", Version)

	select {
	case err = <-errc:
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err = srv.Shutdown(sctx)
	}
	stopPoll()
	<-pollDone
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// newMux serves the probes and the API. Readiness means configuration loaded
// and the store opened, which is true once the server is listening; it
// deliberately does not depend on any target being reachable.
func newMux(apiHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}
