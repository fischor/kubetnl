package main

import (
	"context"
	"github.com/fischor/dew/internal/command"
	"os"
	"os/signal"
)

func main() {
	// Setup ctx. Notifies the client when to stop.
	// Is closed when a os.Interrupt is received.
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		cancel()
	}()

	cmd := command.NewRootCommand(os.Stdin, os.Stdout, os.Stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
