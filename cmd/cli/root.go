// Package cli wires the fleetbox commands.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/provider"

	// Providers register themselves; importing them is what makes a kind available.
	_ "github.com/somaz94/fleetbox/internal/provider/docker"
	_ "github.com/somaz94/fleetbox/internal/provider/ecs"
)

const defaultConfigPath = "/etc/fleetbox/config.yaml"

var (
	// Set via ldflags at build time.
	Version   = "dev"
	GitCommit = "none"
	BuildDate = "unknown"
)

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "fleetbox",
		Short:         "Web console for ECS services and Docker hosts",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newCheckCmd(), newVersionCmd())
	return root
}

// Execute runs the command tree; ctx ends on SIGINT or SIGTERM.
func Execute(ctx context.Context) error {
	if err := NewRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return err
	}
	return nil
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version info",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "fleetbox %s (commit: %s, built: %s)\n", Version, GitCommit, BuildDate)
		},
	}
}

func newCheckCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate the configuration and build every provider without contacting any target",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			targets, err := buildTargets(cfg)
			if err != nil {
				return err
			}
			kinds := map[string]int{}
			for _, t := range targets {
				kinds[t.Provider.Kind()]++
			}
			var parts []string
			for _, k := range provider.Kinds() {
				if n := kinds[k]; n > 0 {
					parts = append(parts, fmt.Sprintf("%s=%d", k, n))
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "config OK: %d targets (%s)\n", len(targets), strings.Join(parts, ", "))
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", defaultConfigPath, "path to the configuration file")
	return cmd
}

func buildTargets(cfg *config.Config) ([]inventory.Target, error) {
	targets := make([]inventory.Target, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		p, err := provider.New(t)
		if err != nil {
			return nil, err
		}
		targets = append(targets, inventory.Target{Config: t, Provider: p})
	}
	return targets, nil
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
