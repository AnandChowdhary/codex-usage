// Command codex-usage reports usage across multiple Codex subscription accounts.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/AnandChowdhary/codex-usage/internal/cli"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := &cli.App{
		Version: version,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Color:   cli.ColorEnabled(os.Stdout),
	}
	code := app.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
