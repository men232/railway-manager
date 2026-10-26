package dbops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
)

// redisArg quotes s for redis-cli's stdin, which splits a line into
// arguments and reads a double-quoted one with backslash escapes. Bytes
// outside printable ASCII become \xHH, so a password cannot end the line.
func redisArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// redisEnv are redis-cli variables of ours the child never gets.
var redisEnv = []string{"REDISCLI_AUTH", "REDISCLI_RCFILE", "REDISCLI_HISTFILE", "REDISCLI_CLUSTER_YES"}

// redisFailure matches what redis-cli prints on stderr while still exiting 0:
// no connection, a refused AUTH, a lost connection.
var redisFailure = regexp.MustCompile(`(?m)^(Could not connect|AUTH failed|Error:)`)

// redisRefused is a refused login in redis-cli's words.
var redisRefused = regexp.MustCompile(`AUTH failed|WRONGPASS|NOAUTH`)

// redisFailed is redis-cli's exit 0 that is a failure: a failure line on
// stderr, or only stderr.
func redisFailed(stdout, stderr string) bool {
	return redisFailure.MatchString(stderr) || (strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) != "")
}

// redisCLI runs one command through the tunnel as user, the password in
// REDISCLI_AUTH and the command on stdin.
func redisCLI(ctx context.Context, r proc.Runner, tun Tunnel, user, password, command string, secrets ...string) (string, error) {
	return redisRun(ctx, r, runClient, tun, user, password, command, secrets...)
}

// redisRun is redisCLI through run: runClient, or runChange for the command
// that changes a password.
func redisRun(ctx context.Context, r proc.Runner, run func(context.Context, proc.Runner, proc.Cmd, time.Duration, judge, ...string) (string, error),
	tun Tunnel, user, password, command string, secrets ...string) (string, error) {
	return run(ctx, r, proc.Cmd{
		Name:  "redis-cli",
		Args:  []string{"-s", tun.Socket, "--user", user, "--no-auth-warning"},
		Env:   []string{"REDISCLI_AUTH=" + password},
		Unset: redisEnv,
		Stdin: command + "\n",
	}, clientTimeout, redisFailed, append(secrets, password)...)
}

var (
	redisOK   = regexp.MustCompile(`(?m)^OK\s*$`)
	redisPong = regexp.MustCompile(`(?m)^PONG\s*$`)
	redisUser = regexp.MustCompile(`(?m)^flags\s*$`)
)

// redisLogin proves password is user's. A Redis that accepts any password
// (no requirepass, a nopass user) would pass any login, so a random password
// is tried first and must be refused.
func redisLogin(ctx context.Context, r proc.Runner, tun Tunnel, user, password string) error {
	probe := make([]byte, 24)
	if _, err := rand.Read(probe); err != nil {
		return err
	}
	out, err := redisCLI(ctx, r, tun, user, hex.EncodeToString(probe), "PING")
	switch {
	case err == nil && redisPong.MatchString(out):
		return fmt.Errorf("redis accepts any password for user %s: a login proves nothing", text.Sanitize(user))
	case err != nil && !redisRefused.MatchString(err.Error()):
		return err // no answer at all: the real login would fare no better
	}
	out, err = redisCLI(ctx, r, tun, user, password, "PING")
	if err != nil {
		return err
	}
	if !redisPong.MatchString(out) {
		return fmt.Errorf("redis-cli: %s", detail(out, "", errors.New("not logged in"), []string{password}))
	}
	return nil
}

// redisChange sets the password of user default with CONFIG SET requirepass,
// of any other user with ACL SETUSER ... resetpass, once ACL GETUSER shows the
// user exists (SETUSER would create it). dry stops before the change.
func redisChange(ctx context.Context, r proc.Runner, tun Tunnel, t Target, adminUser, admin, newPw string, dry bool) error {
	if t.User != "default" {
		out, err := redisCLI(ctx, r, tun, adminUser, admin, "ACL GETUSER "+redisArg(t.User))
		if err != nil {
			return err
		}
		switch {
		case strings.TrimSpace(out) == "": // a nil reply
			return fmt.Errorf("user %s not found", text.Sanitize(t.User))
		case !redisUser.MatchString(out): // an error reply, such as NOAUTH
			return fmt.Errorf("redis-cli: %s", detail(out, "", errors.New("no user record"), []string{admin, newPw}))
		}
	}
	if dry {
		if t.User == "default" {
			return redisLogin(ctx, r, tun, adminUser, admin)
		}
		return nil
	}
	command := "ACL SETUSER " + redisArg(t.User) + " resetpass " + redisArg(">"+newPw)
	if t.User == "default" {
		command = "CONFIG SET requirepass " + redisArg(newPw)
	}
	out, err := redisRun(ctx, r, runChange, tun, adminUser, admin, command, newPw)
	if err != nil {
		return changed(err)
	}
	if !redisOK.MatchString(out) {
		return fmt.Errorf("redis-cli: %s", detail(out, "", errors.New("no OK"), []string{admin, newPw}))
	}
	return nil
}
