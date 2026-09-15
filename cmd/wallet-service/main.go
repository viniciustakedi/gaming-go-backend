// Command wallet-service is the single binary that runs every role of the
// wager wallet: `serve` composes the Fx application (HTTP, and - from later
// tickets - the SQS consumer, outbox publisher and reference worker);
// `migrate` applies or rolls back the schema.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/app"
	"github.com/viniciustakedi/jungle-gaming-wallet/internal/migrate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}

	switch args[0] {
	case "serve":
		return runServe()
	case "migrate":
		return runMigrate(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return usageError()
	}
}

func usageError() error {
	printUsage()
	return fmt.Errorf("unknown command")
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wallet-service serve             start the HTTP server and background components
  wallet-service migrate up        apply every pending migration
  wallet-service migrate down      roll back every applied migration`)
}

// runServe builds the Fx application and blocks until SIGINT/SIGTERM, then
// stops it. SIGTERM triggers the shutdown sequence internal/httpapi,
// internal/pg and internal/queue register: readiness fails, the HTTP server
// drains in-flight requests, and only then do the pool and SQS clients
// release.
func runServe() error {
	fxApp := app.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := fxApp.Start(ctx); err != nil {
		return fmt.Errorf("wallet-service: start: %w", err)
	}

	<-ctx.Done()
	stop()

	stopCtx, cancel := context.WithTimeout(context.Background(), fxApp.StopTimeout())
	defer cancel()
	if err := fxApp.Stop(stopCtx); err != nil {
		return fmt.Errorf("wallet-service: stop: %w", err)
	}
	return nil
}

func runMigrate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("wallet-service migrate: expected exactly one of: up, down")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("wallet-service migrate: DATABASE_URL: required")
	}

	switch args[0] {
	case "up":
		return migrate.Up(dsn)
	case "down":
		return migrate.Down(dsn)
	default:
		return fmt.Errorf("wallet-service migrate: unknown subcommand %q, want up or down", args[0])
	}
}
