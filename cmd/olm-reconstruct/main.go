package main

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := cli.NewRootCommand().ExecuteContext(ctx); err != nil {
		var ee *cli.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		os.Exit(1)
	}
}
