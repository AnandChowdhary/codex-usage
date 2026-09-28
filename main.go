// Command codex-usage reports usage across multiple Codex subscription accounts.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/AnandChowdhary/codex-usage/internal/cli"
)

// version is set by release builds with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := &cli.App{
		Version: buildVersion(),
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Color:   cli.ColorEnabled(os.Stdout),
	}
	code := app.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// buildVersion falls back to the module version Go records for
// `go install ...@v1.2.3` and builds from a tagged checkout.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
}
