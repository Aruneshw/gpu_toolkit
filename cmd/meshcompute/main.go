package main

import (
	"context"
	"fmt"
	"meshcompute/internal/cli"
	"os"
	"os/signal"
)

var version = "0.1.0-dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, version); err != nil {
		fmt.Fprintln(os.Stderr, "meshcompute:", err)
		os.Exit(1)
	}
}
