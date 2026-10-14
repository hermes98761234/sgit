// Command sgd is the sgit per-user object cache daemon.
package main

import (
	"context"
	"os"

	"github.com/hermes98761234/sgit/internal/daemoncmd"
)

func main() {
	os.Exit(daemoncmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
