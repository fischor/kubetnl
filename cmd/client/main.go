package main

import (
	"context"
	"os"

	"github.com/fischor/kubetnl/internal/command"
)

func main() {
	ctx := context.Background()
	cmd := command.NewRootCommand(os.Stdin, os.Stdout, os.Stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
