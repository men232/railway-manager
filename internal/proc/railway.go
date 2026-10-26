// Package proc runs external programs.
package proc

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// RailwayWhoami runs `railway whoami`, which makes the Railway CLI refresh its
// stored token as a side effect. Output is discarded.
func RailwayWhoami(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "railway", "whoami").Run(); err != nil {
		return fmt.Errorf("railway whoami: %w", err)
	}
	return nil
}
