package dbops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
)

// mongoSocket is the tunnel's socket name for Mongo: the driver takes a host
// as a Unix socket path only when it ends in ".sock".
const mongoSocket = "mongodb.sock"

// mongoConnect opens the connection every script starts with. RM_MONGO_URI
// names the user but holds no password; the password comes from
// RM_MONGO_PASSWORD and is spliced in here, inside mongosh, so it is never in
// argv or in a URL on a command line.
const mongoConnect = `
const conn = new Mongo(process.env.RM_MONGO_URI.replace('@', () => ':' + encodeURIComponent(process.env.RM_MONGO_PASSWORD) + '@'));
`

// mongoLoginScript proves the password: the driver authenticates on connect,
// and connectionStatus must name an authenticated user.
const mongoLoginScript = mongoConnect + `
const st = conn.getDB('admin').runCommand({ connectionStatus: 1 });
if (!st.authInfo || st.authInfo.authenticatedUsers.length === 0) { print('NO_AUTH'); quit(2); }
print('OK');
`

// mongoChangeScript changes RM_TARGET_USER's password to RM_NEW_PASSWORD, only
// on the writable primary (else NOT_PRIMARY). The user is the one of
// database RM_TARGET_DB, looked up in admin.system.users: a user of the same
// name in another database is another user. With RM_DRY_RUN=1
// it stops before the change. The change waits for a majority of members, 10
// s at most: a timed-out wait prints WC_TIMEOUT (the primary has the new
// password, the members may not yet).
const mongoChangeScript = mongoConnect + `
const admin = conn.getDB('admin');
if (!admin.runCommand({ hello: 1 }).isWritablePrimary) { print('NOT_PRIMARY'); quit(0); }
const name = process.env.RM_TARGET_USER;
const users = admin.getCollection('system.users');
const found = users.findOne({ user: name, db: process.env.RM_TARGET_DB }, { user: 1, db: 1 });
if (!found) { print('NO_USER'); quit(3); }
if (process.env.RM_DRY_RUN !== '1') {
  try {
    conn.getDB(found.db).updateUser(name, { pwd: process.env.RM_NEW_PASSWORD }, { w: 'majority', wtimeout: 10000 });
  } catch (e) {
    if (e.code === 64 || e.codeName === 'WriteConcernFailed' || e.writeConcernError || /waiting for replication timed out/i.test(String(e.message))) { print('WC_TIMEOUT'); quit(0); }
    throw e;
  }
}
print('OK');
`

// mongoURI is a direct connection to one member through its tunnel socket,
// with user and no password (authSource=authDB, directConnection, 10 s
// timeouts).
func mongoURI(tun Tunnel, user, authDB string) (string, error) {
	if !strings.HasSuffix(tun.Socket, ".sock") {
		return "", errors.New("mongo tunnel socket must end in .sock")
	}
	return "mongodb://" + encodeURIComponent(user) + "@" + encodeURIComponent(tun.Socket) +
		"/?directConnection=true&authSource=" + encodeURIComponent(authDB) + "&connectTimeoutMS=10000&serverSelectionTimeoutMS=10000", nil
}

// mongoEnv are the script's variables: the child never gets them from our
// environment (an RM_DRY_RUN=1 left there would make a change a no-op),
// only from mongoCmd.
var mongoEnv = []string{"RM_MONGO_URI", "RM_MONGO_PASSWORD", "RM_TARGET_USER", "RM_TARGET_DB", "RM_NEW_PASSWORD", "RM_DRY_RUN"}

// mongoCmd runs script against one member as user of authDB with password;
// extra adds environment entries.
func mongoCmd(script string, tun Tunnel, user, authDB, password string, extra ...string) (proc.Cmd, error) {
	uri, err := mongoURI(tun, user, authDB)
	if err != nil {
		return proc.Cmd{}, err
	}
	return proc.Cmd{
		Name:  "mongosh",
		Args:  []string{"--nodb", "--norc", "--quiet", "--eval", script},
		Env:   append([]string{"RM_MONGO_URI=" + uri, "RM_MONGO_PASSWORD=" + password}, extra...),
		Unset: mongoEnv,
	}, nil
}

func mongoLogin(ctx context.Context, r proc.Runner, tun Tunnel, user, authDB, password string) error {
	cmd, err := mongoCmd(mongoLoginScript, tun, user, authDB, password)
	if err != nil {
		return err
	}
	out, err := runClient(ctx, r, cmd, clientTimeout, nil, password)
	if err != nil {
		return err
	}
	if lastLine(out) != "OK" {
		return errors.New("mongosh: not logged in")
	}
	return nil
}

// mongoChange goes through members, each with every admin password in turn,
// until one is the writable primary. A
// member's failure is kept and the next one tried; NO_USER on the primary and
// an incomplete or cancelled run end the search.
func mongoChange(ctx context.Context, r proc.Runner, t Target, members []Tunnel, c Creds, admins []string, dry bool) (bool, error) {
	extra := []string{"RM_TARGET_USER=" + t.User, "RM_TARGET_DB=" + t.authDB(), "RM_DRY_RUN=1"}
	if !dry {
		extra = []string{"RM_TARGET_USER=" + t.User, "RM_TARGET_DB=" + t.authDB(), "RM_NEW_PASSWORD=" + c.New}
	}
	var fails []string
members:
	for i, m := range members {
		for j, admin := range admins {
			cmd, err := mongoCmd(mongoChangeScript, m, c.AdminUser, "admin", admin, extra...)
			if err != nil {
				return false, err
			}
			run := runChange
			if dry {
				run = runClient
			}
			out, err := run(ctx, r, cmd, clientTimeout, nil, c.secrets()...)
			if err != nil && stop(ctx, err) {
				if !dry {
					err = changed(err)
				}
				return false, err
			}
			switch line := lastLine(out); {
			case line == "OK" && err == nil:
				return j == 1, nil
			case line == "NO_USER":
				return false, fmt.Errorf("user %s not found", text.Sanitize(t.User))
			case line == "WC_TIMEOUT" && !dry:
				// The primary took it; a majority did not confirm in time.
				return false, changed(fmt.Errorf("mongosh: a majority of members did not confirm the change in 10 s: %w", ErrIncomplete))
			case line == "NOT_PRIMARY":
				// Logged in: another password would not make it primary.
				fails = append(fails, fmt.Sprintf("member %d: not primary", m.number(i)))
				continue members
			case err == nil:
				err = errors.New("mongosh: unexpected output")
			}
			fails = append(fails, fmt.Sprintf("member %d: %s", m.number(i), err))
		}
	}
	return false, fmt.Errorf("no writable primary: %s", strings.Join(fails, "; "))
}
