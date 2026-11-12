package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/somaz94/fleetbox/cmd/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
