package dbops

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
)

var (
	pgSocket  = regexp.MustCompile(`^\.s\.PGSQL\.(\d+)$`)
	pgVersion = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)`)
)

// pgEnv are the libpq variables that could move psql off the tunnel, change
// how it logs in, or change the client encoding pgIdent quotes for (a
// multibyte one can hide a '"' in a character); the child never gets them
// from our environment.
var pgEnv = []string{"PGHOSTADDR", "PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS", "PGSSLMODE",
	"PGREQUIREAUTH", "PGTARGETSESSIONATTRS", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE",
	"PGCHANNELBINDING", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT", "PGCLIENTENCODING", "PGSYSCONFDIR"}

// psqlProbes caches, per comparable runner, that its psql takes require_auth.
var psqlProbes sync.Map

// psqlMajor is the oldest psql run: 16 brought require_auth, which makes a
// server that does not ask for a SCRAM password, such as one that trusts the
// connection, fail the login instead of passing it.
const psqlMajor = 16

// psqlRecent refuses a local psql older than psqlMajor, naming the command
// that installs a newer one. A success is cached per runner; a failed or
// unreadable probe is an error.
func psqlRecent(ctx context.Context, r proc.Runner) error {
	cacheable := r != nil && reflect.TypeOf(r).Comparable()
	if cacheable {
		if _, ok := psqlProbes.Load(r); ok {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := r.Run(ctx, proc.Cmd{Name: "psql", Args: []string{"--version"}, Unset: pgEnv})
	if err != nil {
		return fmt.Errorf("psql --version: %s", text.Sanitize(err.Error()))
	}
	m := pgVersion.FindStringSubmatch(out)
	if m == nil {
		return errors.New("psql --version: no version in its output")
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return errors.New("psql --version: no version in its output")
	}
	if major < psqlMajor {
		return fmt.Errorf("psql %d is too old: %d or newer is needed to insist on a password login · %s",
			major, psqlMajor, installWith(r, psqlUpgrades, fmt.Sprintf("install psql %d or newer", psqlMajor)))
	}
	if cacheable {
		psqlProbes.Store(r, true)
	}
	return nil
}

// pgQuote is a libpq connection-string value in single quotes.
func pgQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// pgConninfo connects psql through the tunnel's socket directory as user, to
// the always-present postgres database, with a 10 s connect timeout. Every
// setting that could steer the connection elsewhere is pinned (an empty
// hostaddr overrides PGHOSTADDR). The password is not in it: psql reads
// PGPASSWORD. require_auth fails a login the server does not ask a SCRAM
// password for.
func pgConninfo(tun Tunnel, user string) (string, error) {
	m := pgSocket.FindStringSubmatch(filepath.Base(tun.Socket))
	if m == nil {
		return "", errors.New("postgres tunnel socket must be named .s.PGSQL.<port>")
	}
	return fmt.Sprintf("host=%s hostaddr='' port=%s user=%s dbname='postgres' connect_timeout=10 "+
		"sslmode=disable gssencmode=disable target_session_attrs=any require_auth=scram-sha-256", pgQuote(tun.Dir()), m[1], pgQuote(user)), nil
}

// psql runs sql through the tunnel as user, the password in PGPASSWORD and the
// statement on stdin, stopping at the first error. Each of vars, "name=value",
// is a psql variable the statement quotes as :'name'; values are no secrets,
// as they travel in argv.
func psql(ctx context.Context, r proc.Runner, tun Tunnel, user, password string, vars []string, sql string, secrets ...string) (string, error) {
	return psqlRun(ctx, r, runClient, tun, user, password, vars, sql, secrets...)
}

// psqlRun is psql through run: runClient, or runChange for the statement
// that changes a password.
func psqlRun(ctx context.Context, r proc.Runner, run func(context.Context, proc.Runner, proc.Cmd, time.Duration, judge, ...string) (string, error),
	tun Tunnel, user, password string, vars []string, sql string, secrets ...string) (string, error) {
	if err := psqlRecent(ctx, r); err != nil {
		return "", err
	}
	conninfo, err := pgConninfo(tun, user)
	if err != nil {
		return "", err
	}
	args := []string{"-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-d", conninfo}
	for _, v := range vars {
		args = append(args, "-v", v)
	}
	return run(ctx, r, proc.Cmd{
		Name:  "psql",
		Args:  args,
		Env:   []string{"PGPASSWORD=" + password},
		Unset: pgEnv,
		Stdin: sql,
	}, clientTimeout, nil, append(secrets, password)...)
}

func pgLogin(ctx context.Context, r proc.Runner, tun Tunnel, user, password string) error {
	out, err := psql(ctx, r, tun, user, password, nil, "SELECT 1;\n")
	if err != nil {
		return err
	}
	if lastLine(out) != "1" {
		return errors.New("psql: not logged in")
	}
	return nil
}

func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// pgLiteral quotes a value without backslashes (a SCRAM verifier): it does
// not hold under standard_conforming_strings=off, so a name goes in a psql
// variable instead.
func pgLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

const scramIterations = 4096

// scramVerifier is the SCRAM-SHA-256 secret PostgreSQL stores for password,
// computed here as psql's \password does, so the plain password never
// reaches the server and its logs:
// SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey> (RFC 5802, RFC 7677). A
// nil salt means 16 random bytes. There is no SASLprep: a non-ASCII password
// is refused rather than stored in a form the server's login would not match.
func scramVerifier(password string, salt []byte) (string, error) {
	for i := 0; i < len(password); i++ {
		if password[i] >= 0x80 {
			return "", errors.New("postgres: a password with non-ASCII characters is not supported")
		}
	}
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return "", fmt.Errorf("salt: %w", err)
		}
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("scram: %w", err)
	}
	mac := func(msg string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac("Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramIterations, b64(salt), b64(stored[:]), b64(mac("Server Key"))), nil
}

// pgUserExists asks as the admin whether t.User is a role. The name goes in
// the psql variable u, which psql quotes for the server's own string rules
// (standard_conforming_strings off included).
func pgUserExists(ctx context.Context, r proc.Runner, tun Tunnel, t Target, adminUser, admin string, secrets ...string) error {
	out, err := psql(ctx, r, tun, adminUser, admin, []string{"u=" + t.User},
		"SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'u') THEN 'OK' ELSE 'NO_USER' END;\n", secrets...)
	if err != nil {
		return err
	}
	if lastLine(out) != "OK" {
		return fmt.Errorf("user %s not found", text.Sanitize(t.User))
	}
	return nil
}

// pgChange is one ALTER USER as the admin carrying the SCRAM verifier of the
// new password, after a check that the user exists (for a user other than
// the admin's root); dry stops after the check.
func pgChange(ctx context.Context, r proc.Runner, tun Tunnel, t Target, adminUser, admin, newPw string, dry bool) error {
	verifier, err := scramVerifier(newPw, nil)
	if err != nil {
		return err
	}
	if dry || !t.Root {
		if err := pgUserExists(ctx, r, tun, t, adminUser, admin, newPw); err != nil || dry {
			return err
		}
	}
	_, err = psqlRun(ctx, r, runChange, tun, adminUser, admin, nil, "ALTER USER "+pgIdent(t.User)+" WITH PASSWORD "+pgLiteral(verifier)+";\n", newPw, verifier)
	return changed(err)
}
