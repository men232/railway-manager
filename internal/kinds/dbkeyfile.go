package kinds

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/dbops"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
)

const kindDBKeyFile = "db.keyfile"

// KeyFileMax is the longest KEYFILE Mongo takes.
const KeyFileMax = 1024

// CheckKeyFile refuses a KEYFILE Mongo cannot take: 6 to 1024 base64
// characters (letters, digits, + / =). The decision, and every write of a
// KEYFILE group whoever made it (variable.set ValidateData), share it. The
// error never holds the value.
func CheckKeyFile(v string) error {
	if len(v) < 6 || len(v) > KeyFileMax {
		return errors.New("a KEYFILE is 6 to 1024 characters")
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return errors.New("a KEYFILE holds base64 characters only (letters, digits, + / =)")
		}
	}
	return nil
}

// keyFileGroup reports whether group is a KEYFILE row's group.
func keyFileGroup(group string) bool {
	_, user, ok := rotation.ParseDBGroupKey(group)
	return ok && user == rotation.KeyFileUser
}

// keyFileNew is the New of a db.keyfile change: the database only. The new
// KEYFILE value travels in the group's variable.set changes.
type keyFileNew struct {
	DB dbInfo `json:"db"`
}

// DBKeyFile makes the db.keyfile change of a Mongo KEYFILE row: place the
// owner, target rotation.KeyFileUser, group the row's. With it the owner and
// every member redeploy together, and a verify checks the set has a
// writable primary again. The KEYFILE values themselves
// are variable.set changes of the same group.
func DBKeyFile(s *store.Store, row rotation.PasswordRow, now time.Time) (changeset.Change, error) {
	if !row.KeyFile {
		return changeset.Change{}, errors.New("not a KEYFILE row")
	}
	if row.Problem != "" {
		return changeset.Change{}, errors.New(row.Problem)
	}
	info := infoOf(s, row)
	info.After = nil
	raw, err := json.Marshal(keyFileNew{DB: info})
	if err != nil {
		return changeset.Change{}, err
	}
	c := changeset.Change{
		ID: changeset.NewID(), Kind: kindDBKeyFile, Place: row.DB.Owner, Target: rotation.KeyFileUser,
		New: raw, Group: row.Key, Created: now,
	}
	if err := (dbKeyFileKind{}).Validate(s, c); err != nil {
		return changeset.Change{}, err
	}
	return c, nil
}

func decodeKeyFile(c changeset.Change) (dbInfo, error) {
	var nv keyFileNew
	if err := strictDecode(c.New, &nv); err != nil {
		return dbInfo{}, errors.New("db.keyfile: bad change data")
	}
	if nv.DB.Kind != "mongo" {
		return dbInfo{}, errors.New("db.keyfile: only a Mongo database has a KEYFILE")
	}
	if err := nv.DB.valid(); err != nil {
		return dbInfo{}, errors.New("db.keyfile: bad change data: " + err.Error())
	}
	if c.Place.Shared() || c.Target != rotation.KeyFileUser || c.Group != rotation.DBGroupKey(c.Place.Key(), rotation.KeyFileUser) {
		return dbInfo{}, errors.New("db.keyfile: bad change data: target")
	}
	return nv.DB, nil
}

// dbKeyFileKind is db.keyfile: a Mongo replica set restarted with a new
// KEYFILE, then verified.
type dbKeyFileKind struct{}

func (dbKeyFileKind) Name() string { return kindDBKeyFile }

func (dbKeyFileKind) ValidateData(c changeset.Change) error {
	_, err := decodeKeyFile(c)
	return err
}

// Validate is ValidateData, and refuses the change while its row, as the
// store holds it now, is gone (fails closed: its copies cannot be checked)
// or has a Problem (a KEYFILE copy outside the members).
func (k dbKeyFileKind) Validate(s *store.Store, c changeset.Change) error {
	if err := k.ValidateData(c); err != nil {
		return err
	}
	row, ok := rotation.RowOf(s, c.Group, rotation.Options{EnvIDs: []string{c.Place.EnvID}})
	switch {
	case !ok:
		return errors.New("the KEYFILE's database is not found · refresh")
	case row.Problem != "":
		return errors.New(row.Problem)
	}
	return nil
}

func (dbKeyFileKind) IsNoop(changeset.Change) bool { return false }
func (dbKeyFileKind) Secret(changeset.Change) bool { return false }

func (dbKeyFileKind) Overlay(changeset.Change) apply.Overlay {
	s := "→ new keyfile · members redeploy"
	return apply.Overlay{Short: s, Long: s, Tone: "warn"}
}

func (dbKeyFileKind) Writes(c changeset.Change) []string {
	return []string{dbWrite(c.Place, rotation.KeyFileUser)}
}
func (dbKeyFileKind) Reads(changeset.Change) []string { return nil }

func (dbKeyFileKind) Check(context.Context, *apply.Env, changeset.Change) (apply.Drift, error) {
	return apply.DriftNone, nil
}
func (dbKeyFileKind) Patch(changeset.Change) (map[string]any, bool) { return nil, false }

// Redeploys: the owner and every member, together.
func (dbKeyFileKind) Redeploys(_ *store.Store, c changeset.Change) []domain.Place {
	info, err := decodeKeyFile(c)
	if err != nil {
		return nil
	}
	return slices.Concat([]domain.Place{c.Place}, info.Members)
}

// OwnSteps: the verify, after the members' redeploys.
func (dbKeyFileKind) OwnSteps(c changeset.Change) []apply.Step {
	return []apply.Step{&dbStep{phase: stepDBVerify, c: c, serves: true}}
}

var _ apply.DataValidator = dbKeyFileKind{}

// verifyKeyFile checks that the restarted set has a writable primary, as
// the root with the current root password, repairing a single member lost
// from its own config (dbops.VerifyMongo); a repair is the outcome's
// Reason. It needs every member's tunnel.
func (s *dbStep) verifyKeyFile(ctx context.Context, env *apply.Env) (apply.Result, error) {
	info, err := decodeKeyFile(s.c)
	if err != nil {
		return apply.Result{}, err
	}
	pw, err := currentRoot(ctx, env, s.c.Place, info.Kind)
	if err != nil {
		return apply.Result{}, err
	}
	sleep := env.Sleep
	if sleep == nil {
		sleep = dbops.Sleep
	}
	var res dbops.Result
	err = withTunnels(ctx, env, s.Key(), s.c.Place, info, true, func(tuns []dbops.Tunnel) error {
		var err error
		res, err = dbops.VerifyMongo(ctx, env.Proc, info.RootUser, tuns, pw, sleep)
		return err
	})
	if err != nil {
		return apply.Result{}, err
	}
	out := s.done()
	if res.Outcome == dbops.Repaired {
		// A repaired node is reported, not hidden.
		out.Outcomes[s.c.ID] = apply.Outcome{Status: "done", Reason: res.String()}
	}
	return out, nil
}
