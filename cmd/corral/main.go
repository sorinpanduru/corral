// Command corral is a MongoDB wire-protocol connection-pooling proxy.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sorinpanduru/corral/internal/app/runtime"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := runtime.Run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, runtime.Hooks{})
	stop()
	os.Exit(code)
}
