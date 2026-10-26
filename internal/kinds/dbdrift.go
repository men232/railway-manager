package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
)

// A write of a database password group whose target changed in Railway
// since it was made: an automatic URL rewrite
// follows the current URL when it still has the row's user and the
// rewrite's hosts (rebaseRewrite); anything else is refused before the
// database is touched (checkGroupWrites) and fails its patch after
// (driftNote), so an app is never left on the old password while the run
// reads finished.

// dbGroupWrite reports whether c is a variable write of a database password
// group.
func dbGroupWrite(c changeset.Change) bool {
	return (c.Kind == kindSet || c.Kind == kindDelete) && strings.HasPrefix(c.Group, apply.DBGroupPrefix)
}

// groupPassword is the new password of the run's db.password change of
// group (env.Changes); "" when there is none, or it is a revert.
func groupPassword(env *apply.Env, group string) string {
	for _, c := range env.Changes {
		if c.Group != group {
			continue
		}
		if src, v, ok := PasswordValue(c); ok && src != SourceRevert {
			return v
		}
	}
	return ""
}

// rebaseRewrite is c, an automatic URL rewrite of a password group whose
// target drifted, re-made on the value Railway holds now (rotation.RebaseURL)
// with its base set to that value. ok is false when c is no such rewrite, or
// the current value is gone, unreadable or no longer a clear URL of the
// row's user on the rewrite's hosts. It reads the place through env's cache.
func rebaseRewrite(ctx context.Context, env *apply.Env, c changeset.Change) (changeset.Change, bool, error) {
	if c.Kind != kindSet || !rotation.Automatic(c) || c.Base.Sealed || c.Base.Absent {
		return c, false, nil
	}
	_, user, ok := rotation.ParseDBGroupKey(c.Group)
	if !ok || user == rotation.KeyFileUser {
		return c, false, nil
	}
	password := groupPassword(env, c.Group)
	nv, err := validNew(c)
	if password == "" || err != nil || nv.Copy != nil || len(nv.Refs) > 0 {
		return c, false, nil
	}
	vars, err := env.ReadPlace(ctx, c.Place)
	if err != nil {
		return c, false, err
	}
	cur, present := vars[c.Target]
	if !present {
		return c, false, nil
	}
	next, ok := rotation.RebaseURL(cur, nv.Value, user, password)
	if !ok {
		return c, false, nil
	}
	raw, err := json.Marshal(newValue{Value: next})
	if err != nil {
		return c, false, err
	}
	c.New = raw
	c.Base = changeset.Base{Fingerprint: fingerprint(c, cur)}
	return c, true, nil
}

// writeLabel names a write's place: "<env> · <service> <NAME>" when env has a
// store, else "<NAME> at <place key>". Never a value.
func writeLabel(env *apply.Env, c changeset.Change) string {
	if env.Store == nil {
		return text.Sanitize(label(c))
	}
	svc := "shared"
	if !c.Place.Shared() {
		svc = env.Store.ServiceName(c.Place.ServiceID)
	}
	return text.Sanitize(env.Store.EnvName(c.Place.EnvID)) + " · " + text.Sanitize(svc) + " " + text.Sanitize(c.Target)
}

// checkGroupWrites checks, writing nothing, every write of main's password
// group in the run (env.Changes) against Railway, for the pre-flight: a
// target changed since the write was made refuses the run, naming the place,
// unless the write is an automatic rewrite the patch re-bases. The places it
// reads are forgotten after, so the patch reads Railway again.
func checkGroupWrites(ctx context.Context, env *apply.Env, main changeset.Change) error {
	var read []domain.Place
	defer func() {
		for _, p := range read {
			env.Forget(p)
		}
	}()
	for _, c := range env.Changes {
		if c.ID == main.ID || c.Group != main.Group || !dbGroupWrite(c) {
			continue
		}
		read = append(read, c.Place)
		d, err := (variableKind{del: c.Kind == kindDelete}).Check(ctx, env, c)
		if err != nil {
			return fmt.Errorf("check %s: %w", writeLabel(env, c), err)
		}
		if d != apply.DriftChanged {
			continue
		}
		_, ok, err := rebaseRewrite(ctx, env, c)
		switch {
		case err != nil:
			return fmt.Errorf("check %s: %w", writeLabel(env, c), err)
		case ok:
			continue
		}
		return errors.New(writeLabel(env, c) + " " + driftWhat(c) + " · decide it again")
	}
	return nil
}

// driftNote is why a write of a password group the patch cannot make fails
// its step after the database changed: the user either abandons the run
// (the place keeps what it holds) or writes the new password there.
func driftNote(env *apply.Env, c changeset.Change) string {
	if c.Kind == kindDelete {
		return writeLabel(env, c) + ": deleted in the decision · delete it in Railway, or abandon to keep it"
	}
	return writeLabel(env, c) + ": " + driftWhat(c) + " · abandon to keep it, or set it in Railway to the new password"
}

// driftWhat says how a write's target drifted: an automatic URL rewrite
// that cannot follow it, or a decision (a place's own value, the database's
// own variable).
func driftWhat(c changeset.Change) string {
	if rotation.Automatic(c) && c.Kind == kindSet && rotation.IsCredURL(decodeNew(c).Value) {
		return "changed in Railway and no longer has this user and host"
	}
	return "changed in Railway since the decision"
}
