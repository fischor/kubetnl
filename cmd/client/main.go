package main

import (
	"context"
	"github.com/fischor/dew/internal/command"
	"os"
)

func main() {
	ctx := context.Background()
	cmd := command.NewRootCommand(os.Stdin, os.Stdout, os.Stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
