// Package dbops changes database passwords through SSH tunnels into a
// service's own container.
package dbops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
)

const sshConfigTimeout = 60 * time.Second

var (
	sshUser = regexp.MustCompile(`(?m)^\s*User\s+(\S+)`)
	sshHost = regexp.MustCompile(`(?m)^\s*HostName\s+(\S+)`)
)

// SSHTarget returns the `User@HostName` of a service's SSH endpoint, read from
// the config block `railway ssh config --dry-run` prints (step 1). It writes
// nothing. A CLI failure is reported as such; output without a User or
// HostName means the service has no SSH access. A value starting with "-" is
// refused: it would reach ssh's argv as an option.
func SSHTarget(ctx context.Context, r proc.Runner, projectID, envID, serviceID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sshConfigTimeout)
	defer cancel()
	out, _, err := r.Run(ctx, proc.Cmd{
		Name: "railway",
		Args: []string{"ssh", "config", "--dry-run", "-p", projectID, "-e", envID, "-s", serviceID},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("railway ssh config: %w", err)
		}
		return "", fmt.Errorf("railway ssh config: %s", text.Sanitize(err.Error()))
	}
	user, host := sshUser.FindStringSubmatch(out), sshHost.FindStringSubmatch(out)
	if user == nil || host == nil {
		return "", fmt.Errorf("no SSH access to %s", text.Sanitize(serviceID))
	}
	u, h := text.Sanitize(user[1]), text.Sanitize(host[1])
	if strings.HasPrefix(u, "-") || strings.HasPrefix(h, "-") {
		return "", fmt.Errorf("unexpected ssh config for %s", text.Sanitize(serviceID))
	}
	return u + "@" + h, nil
}
