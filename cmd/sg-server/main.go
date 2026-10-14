// Command sg-server is the sgit gRPC server over bare git repositories.
package main

import (
	"context"
	"os"

	"github.com/hermes98761234/sgit/internal/servercmd"
)

func main() {
	os.Exit(servercmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
