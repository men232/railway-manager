package tuitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
)

// StalledRun writes run id under dir/runs, as an apply would leave it:
// failed after its database step changed the password of user "root" of
// the database on owner. Its frozen set holds the db.password change
// (carrying secret, the new password; id "<id>-pw") and the rewrite at
// other (id "<id>-url"), their redeploys frozen — the database owner's
// done, the app other's failed (1 of 1 apps not redeployed); the verify
// is blocked. It returns the group.
func StalledRun(t *testing.T, dir, id string, created time.Time, owner, other domain.Place, secret string) string {
	t.Helper()
	d := filepath.Join(dir, "runs", id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	group := changeset.PasswordGroupPrefix + owner.Key() + ":root"
	sfx := owner.Key() + ":root"
	nv, _ := json.Marshal(map[string]string{"source": "generate", "value": secret})
	url, _ := json.Marshal(map[string]string{"value": "mongodb://root:" + secret + "@mongo.railway.internal:27017"})
	frozen := changeset.ChangeSet{Changes: []changeset.Change{
		{ID: id + "-pw", Kind: "db.password", Place: owner, Target: "root", Group: group, New: nv, Created: created},
		{ID: id + "-url", Kind: "variable.set", Place: other, Target: "DB_URL", Group: group, New: url, Created: created},
	}}
	if err := changeset.Save(filepath.Join(d, "changeset.json"), frozen); err != nil {
		t.Fatal(err)
	}
	actions, _ := json.Marshal([]apply.FrozenAction{
		{Key: "redeploy:" + owner.Key(), Place: owner, Serves: []string{id + "-pw"}},
		{Key: "redeploy:" + other.Key(), Place: other, Serves: []string{id + "-url"}},
	})
	if err := os.WriteFile(filepath.Join(d, "actions.json"), actions, 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := apply.OpenJournal(filepath.Join(d, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	j.Created = created
	for k, rec := range map[string]apply.StepRecord{
		"preflight:" + sfx:        {Status: apply.StatusDone, Acted: true},
		"dbpw:" + sfx:             {Status: apply.StatusDone, Acted: true},
		"patch:" + other.EnvID:    {Status: apply.StatusDone, Acted: true},
		"redeploy:" + other.Key(): {Status: apply.StatusFailed},
		"redeploy:" + owner.Key(): {Status: apply.StatusDone, Acted: true},
		"dbverify:" + sfx:         {Status: apply.StatusBlocked},
	} {
		rec.At = created
		if err := j.Set(k, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.SetState(apply.RunFailed); err != nil {
		t.Fatal(err)
	}
	return group
}
