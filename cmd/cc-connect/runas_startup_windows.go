//go:build windows

package main

import (
	"context"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/config"
)

func runRunAsUserStartupChecks(_ context.Context, _ *config.Config) error {
	return nil
}
