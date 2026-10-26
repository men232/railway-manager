package dbops

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
)

// Target is the database user whose password is proved or changed.
type Target struct {
	DB   rotation.DB
	User string
	// Root is set when User is the database's root user.
	Root bool
	// AuthDB is the Mongo database the user belongs to, the one its URLs
	// authenticate against; "" is admin.
	AuthDB string
}

// authDB is the Mongo database of the target user: AuthDB, else admin.
func (t Target) authDB() string {
	if t.AuthDB == "" {
		return "admin"
	}
	return t.AuthDB
}

// Creds are the passwords of one change. They are never logged, never put in
// a child's argv and never left in an error.
type Creds struct {
	// AdminUser logs in to make the change.
	AdminUser string
	// Admin is the admin's password for a change of another user's password.
	// It is empty for a root change, whose admin passwords are Current, then
	// New.
	Admin string
	// Current is the password before the change; New the one to set.
	Current, New string
}

func (c Creds) secrets() []string { return []string{c.Admin, c.Current, c.New} }

// admins are the passwords to log in with, in order: for a root change the
// current password, then the new one, which covers a run interrupted after
// the database took the new password.
func (c Creds) admins(t Target) ([]string, error) {
	if c.AdminUser == "" || c.New == "" {
		return nil, errNoCreds
	}
	if !t.Root {
		if c.Admin == "" {
			return nil, errNoCreds
		}
		return []string{c.Admin}, nil
	}
	if c.Current == "" {
		return nil, errNoCreds
	}
	if c.Current == c.New {
		return []string{c.New}, nil
	}
	return []string{c.Current, c.New}, nil
}

var errNoCreds = errors.New("current credentials could not be read from Railway")

// ErrIncomplete is a client whose output may have been cut short: after a
// password change, the database may hold the new password even though no
// success was read.
var ErrIncomplete = errors.New("output may be incomplete")

const (
	clientTimeout = 60 * time.Second
	verifyTimeout = 90 * time.Second
	detailMax     = 400
)

// retrySleep is the wait between transient retries; tests replace it.
var retrySleep = Sleep

// clientOf is the program each database kind is reached with.
var clientOf = map[string]string{"mongo": "mongosh", "postgres": "psql", "redis": "redis-cli", "ssh": "ssh"}

// managers in detection order.
var managers = []string{"brew", "apt-get", "dnf", "apk"}

// installs are the install commands per client and package manager;
// mongosh is not in distro repositories.
var installs = map[string]map[string]string{
	"mongosh": {
		"brew":    "brew install mongosh",
		"apt-get": "npm install -g mongosh",
		"dnf":     "npm install -g mongosh",
		"apk":     "npm install -g mongosh",
	},
	"psql": {
		"brew":    "brew install libpq && brew link --force libpq",
		"apt-get": "apt-get install postgresql-client",
		"dnf":     "dnf install postgresql",
		"apk":     "apk add postgresql",
	},
	"redis-cli": {
		"brew":    "brew install redis",
		"apt-get": "apt-get install redis-tools",
		"dnf":     "dnf install redis",
		"apk":     "apk add redis",
	},
	"ssh": {
		"brew":    "brew install openssh",
		"apt-get": "apt-get install openssh-client",
		"dnf":     "dnf install openssh-clients",
		"apk":     "apk add openssh-client",
	},
}

// psqlUpgrades install a psql of 16 or later, the first with require_auth.
var psqlUpgrades = map[string]string{
	"brew":    "brew install libpq && brew link --force libpq",
	"apt-get": "apt-get install postgresql-client-16",
	"dnf":     "dnf install postgresql16",
	"apk":     "apk add postgresql16-client",
}

// Missing reports whether the client a database kind ("mongo", "postgres",
// "redis", or "ssh" for the tunnel) needs is absent from PATH, and if so the
// command that installs it with the first package manager found (brew,
// apt-get, dnf, apk), or a generic "install <client>". An unknown kind is an
// error.
func Missing(r proc.Runner, kind string) (install string, missing bool, err error) {
	client, ok := clientOf[kind]
	if !ok {
		return "", false, fmt.Errorf("unknown database kind %s", text.Sanitize(kind))
	}
	if _, err := r.LookPath(client); err == nil {
		return "", false, nil
	}
	return installWith(r, installs[client], "install "+client), true, nil
}

// installWith is the command in cmds for the first package manager found,
// else fallback.
func installWith(r proc.Runner, cmds map[string]string, fallback string) string {
	for _, m := range managers {
		if _, err := r.LookPath(m); err == nil {
			return cmds[m]
		}
	}
	return fallback
}

// Forward is where a database kind's tunnel goes: the host inside the
// container and the socket file name its client needs. Postgres always
// forwards to the container's private address (RAILWAY_PRIVATE_DOMAIN):
// Railway's template trusts 127.0.0.1, so a login there proves no password.
// Mongo's socket must end in ".sock" for its driver;
// psql looks for ".s.PGSQL.<port>" in the directory it is given.
func Forward(kind string, port int, privateHost string) (remoteHost, socketName string, err error) {
	switch kind {
	case "mongo":
		return "127.0.0.1", mongoSocket, nil
	case "redis":
		return "127.0.0.1", DefaultSocketName, nil
	case "postgres":
		if privateHost == "" || strings.HasPrefix(privateHost, "-") || strings.ContainsAny(privateHost, ": \t\n") {
			return "", "", errors.New("postgres: no private address to prove the password on")
		}
		return privateHost, fmt.Sprintf(".s.PGSQL.%d", port), nil
	}
	return "", "", fmt.Errorf("unknown database kind %s", text.Sanitize(kind))
}

// Login proves that password is t.User's: it logs in through tunnel and runs a
// harmless command. A Postgres tunnel must come from Forward, so the login
// reaches the private address. The error never holds the password.
func Login(ctx context.Context, r proc.Runner, t Target, tunnel *Tunnel, password string) (err error) {
	defer func() { err = scrub(err, password) }()
	if tunnel == nil {
		return errors.New("no tunnel")
	}
	if t.User == "" || password == "" {
		return errNoCreds
	}
	switch t.DB.Kind {
	case "mongo":
		return mongoLogin(ctx, r, *tunnel, t.User, t.authDB(), password)
	case "postgres":
		return pgLogin(ctx, r, *tunnel, t.User, password)
	case "redis":
		return redisLogin(ctx, r, *tunnel, t.User, password)
	}
	return fmt.Errorf("unknown database kind %s", text.Sanitize(t.DB.Kind))
}

// SetPassword sets t.User's password to c.New inside the database. A root
// change logs in with c.Current, then c.New (usedNew: the database already
// had it, so the change was a no-op); another user's change logs in as
// c.AdminUser with c.Admin. Mongo goes through members until one is the
// writable primary; Postgres and Redis use members[0]. An error wrapping
// ErrIncomplete means the change may have happened.
func SetPassword(ctx context.Context, r proc.Runner, t Target, members []Tunnel, c Creds) (usedNew bool, err error) {
	return change(ctx, r, t, members, c, false)
}

// Check is SetPassword without the change: it proves the admin login, finds
// the writable primary (Mongo) and that t.User exists. usedNew as SetPassword.
func Check(ctx context.Context, r proc.Runner, t Target, members []Tunnel, c Creds) (usedNew bool, err error) {
	return change(ctx, r, t, members, c, true)
}

func change(ctx context.Context, r proc.Runner, t Target, members []Tunnel, c Creds, dry bool) (usedNew bool, err error) {
	defer func() { err = scrub(err, c.secrets()...) }()
	admins, err := c.admins(t)
	if err != nil {
		return false, err
	}
	if t.User == "" {
		return false, errNoCreds
	}
	if len(members) == 0 {
		return false, errors.New("no tunnel")
	}
	var do func(ctx context.Context, r proc.Runner, tun Tunnel, t Target, adminUser, admin, newPw string, dry bool) error
	switch t.DB.Kind {
	case "mongo":
		return mongoChange(ctx, r, t, members, c, admins, dry)
	case "postgres":
		do = pgChange
	case "redis":
		do = redisChange
	default:
		return false, fmt.Errorf("unknown database kind %s", text.Sanitize(t.DB.Kind))
	}
	if t.DB.Kind == "postgres" {
		// Refuse a password the verifier cannot carry before any login.
		if _, err := scramVerifier(c.New, make([]byte, 16)); err != nil {
			return false, err
		}
	}
	var errs []error
	for i, admin := range admins {
		err := do(ctx, r, members[0], t, c.AdminUser, admin, c.New, dry)
		if err == nil {
			return i == 1, nil
		}
		if stop(ctx, err) {
			return false, err
		}
		errs = append(errs, err)
	}
	return false, joinTries(errs)
}

// changed marks an incomplete run of the change itself: the database may
// hold the new password although no success was read.
func changed(err error) error {
	if errors.Is(err, ErrIncomplete) {
		return fmt.Errorf("%w; the database may already have the new password", err)
	}
	return err
}

// stop is an error after which no other login is tried: a cancelled run, or
// a change whose outcome is unknown.
func stop(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, ErrIncomplete)
}

func joinTries(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		label := "current password"
		if i == 1 {
			label = "new password"
		}
		msgs[i] = label + ": " + e.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}

// runClient runs a client, again on each transient failure, and returns its
// stdout, also on failure.
func runClient(ctx context.Context, r proc.Runner, cmd proc.Cmd, timeout time.Duration, failed judge, secrets ...string) (string, error) {
	return runRetried(ctx, r, cmd, timeout, failed, false, secrets)
}

// runChange is runClient for the call that changes a password: our own
// timeout is no transient there — the change may have reached the
// database — so it wraps ErrIncomplete (never retried, the outcome
// unknown) instead.
func runChange(ctx context.Context, r proc.Runner, cmd proc.Cmd, timeout time.Duration, failed judge, secrets ...string) (string, error) {
	return runRetried(ctx, r, cmd, timeout, failed, true, secrets)
}

func runRetried(ctx context.Context, r proc.Runner, cmd proc.Cmd, timeout time.Duration, failed judge, changes bool, secrets []string) (string, error) {
	var stdout string
	_, err := Retry(ctx, retrySleep, func() (string, error) {
		out, raw, err := runOnce(ctx, r, cmd, timeout, failed, secrets)
		stdout = out
		var te *timeoutError
		if changes && errors.As(err, &te) {
			err = fmt.Errorf("%w (%w)", te, ErrIncomplete)
		}
		return raw, err
	})
	return stdout, err
}

// timeoutError is a client run our own timeout ended.
type timeoutError struct {
	name string
	secs int
}

func (e *timeoutError) Error() string { return fmt.Sprintf("%s timed out after %d s", e.name, e.secs) }

// judge tells a failure in the output of a client that exited 0 (redis-cli);
// nil trusts the exit code.
type judge func(stdout, stderr string) bool

// runOnce runs a client bounded by timeout. It returns stdout, the raw output
// for transient matching, and an error naming the client with at most 400
// characters of its output, already redacted of secrets. An
// exec.ErrWaitDelay becomes ErrIncomplete: the output may have been cut.
func runOnce(ctx context.Context, r proc.Runner, cmd proc.Cmd, timeout time.Duration, failed judge, secrets []string) (stdout, raw string, err error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, errOut, err := r.Run(cctx, cmd)
	switch {
	case err == nil && failed != nil && failed(out, errOut):
		err = errors.New("failed")
	case err == nil:
		return out, out, nil
	case errors.Is(err, exec.ErrWaitDelay):
		return out, "", fmt.Errorf("%s: %w", cmd.Name, ErrIncomplete)
	case ctx.Err() != nil:
		return out, "", fmt.Errorf("%s: %w", cmd.Name, ctx.Err())
	case errors.Is(err, context.DeadlineExceeded):
		return out, "", &timeoutError{name: cmd.Name, secs: int(timeout / time.Second)}
	}
	// psql starts its own messages with "psql: ".
	d := strings.TrimPrefix(detail(out, errOut, err, secrets), cmd.Name+": ")
	return out, out + "\n" + errOut, fmt.Errorf("%s: %s", cmd.Name, d)
}

// quotedStatement is psql's echo of the failing statement: "LINE 1: ..." and
// the caret under it. It may quote a password, so it is dropped.
var quotedStatement = regexp.MustCompile(`^\s*(LINE \d+:.*|\^\s*)$`)

// detail is the client's own words on a failure: stderr, else stdout, else
// the error, without psql's statement echo, redacted, then cut to 400
// characters.
func detail(stdout, stderr string, err error, secrets []string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		s = strings.TrimSpace(stdout)
	}
	if s == "" {
		s = err.Error()
	}
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if !quotedStatement.MatchString(l) && strings.TrimSpace(l) != "" {
			keep = append(keep, strings.TrimSpace(l))
		}
	}
	s = text.Sanitize(redact(strings.Join(keep, "\n"), secrets))
	if rs := []rune(s); len(rs) > detailMax {
		s = string(rs[:detailMax]) + "…"
	}
	return s
}

// lastLine is the last non-empty line of a client's stdout.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
