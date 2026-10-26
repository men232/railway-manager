package kinds

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
)

// NewPasswordChanges builds every change of one password decision, all in the
// row's group (rotation.DBGroupKey):
//
//   - the db.password change (db.keyfile for a KEYFILE row);
//   - the database's own variable set to value (a root or the KEYFILE);
//   - per rewritten URL place without a decision, a variable.set of its
//     current URL with the password replaced (rotation.RewriteURL), resolved
//     now; literal copies (a KEYFILE's, a password's) get value itself;
//   - per place the user decided in pending (p, f, c, x or k: a change of
//     the group not marked automatic), that decision; an unrecognised
//     place takes nothing else. A set carries the new
//     password, so it Requires the db.password change; a delete or a keep
//     does not.
//
// The changes it makes itself carry Origin{Producer "rotate", Row the
// group, Letter changeset.LetterRewrite}: deciding the password again
// rebuilds them with the new password, and removing the password change
// removes them (PasswordFollowers). Every password write Requires the
// db.password change, a hand source's too (it then waits on the
// pre-flight); KEYFILE writes require nothing.
//
// Refused: a row with a Problem; a typed password CheckPastedPassword
// refuses; an undecided rewritten place whose URL cannot be rewritten
// safely (Unclear: set it by hand first) or an undecided unrecognised
// place (rotation.PlaceRefusal); a place holding a pending change
// outside the group (an edit made elsewhere, or a value rotation), which
// the decision would silently replace. When pending holds the row's
// password change, the new one takes its id, so the Requires stay true once
// the set merges it. The value is a secret; errors never hold it.
func NewPasswordChanges(s *store.Store, pending []changeset.Change, row rotation.PasswordRow, source, value string, now time.Time) ([]changeset.Change, error) {
	if row.Problem != "" {
		return nil, errors.New(text.Sanitize(row.Problem))
	}
	var main changeset.Change
	var err error
	if row.KeyFile {
		if value == "" {
			return nil, errors.New("no new KEYFILE")
		}
		if err := CheckKeyFile(value); err != nil {
			return nil, err
		}
		main, err = DBKeyFile(s, row, now)
	} else {
		check := CheckPastedPassword(row.DB.Kind, value)
		if source == SourceHand {
			check = CheckTypedPassword(value)
		}
		if source != SourceGenerate && check != nil {
			return nil, check
		}
		main, err = DBPassword(s, row, source, value, now)
	}
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(pending, func(c changeset.Change) bool { return c.Key() == main.Key() && c.Kind == main.Kind }); i >= 0 {
		main.ID = pending[i].ID
	}
	var requires []string
	if !row.KeyFile {
		requires = []string{main.ID}
	}
	auto := func(c *changeset.Change) {
		c.Origin = &changeset.Origin{Producer: rotateProducer, Row: row.Key, Letter: changeset.LetterRewrite}
	}
	auto(&main)
	out := []changeset.Change{main}
	add := func(c changeset.Change, needs bool) {
		c.Group, c.Requires = row.Key, nil
		if needs {
			c.Requires = slices.Clone(requires)
		}
		out = append(out, c)
	}

	if row.Own != nil {
		label := placeLabel(s, row.Own.Place, row.Own.Name)
		if msg := rotation.PlaceRefusal(s, row, rotation.URLPlace{Loc: *row.Own}, pending, nil, false); msg != "" {
			return nil, errors.New(msg)
		}
		c, err := SetVariable(s, pending, row.Own.Place, row.Own.Name, value, now)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		auto(&c)
		add(c, true)
	}
	for _, u := range slices.Concat(row.URLs, row.Copies) {
		if u.Mode != rotation.URLRewrite && u.Mode != rotation.URLUnrecognised {
			continue
		}
		label := placeLabel(s, u.Loc.Place, u.Loc.Name)
		if msg := rotation.PlaceRefusal(s, row, u, pending, nil, false); msg != "" {
			return nil, errors.New(msg)
		}
		if d, ok := pendingAt(pending, u.Loc); ok && !automatic(d) {
			add(d, d.Kind == kindSet)
			continue
		}
		if u.Mode == rotation.URLUnrecognised {
			continue // PlaceRefusal: decided by hand only
		}
		next := value
		if !row.KeyFile && !u.Copy {
			v := s.Variable(u.Loc.ID)
			if v == nil || v.Sealed || !v.Literal {
				return nil, errors.New(label + ": cannot be read · set this place by hand")
			}
			if next, err = rotation.RewriteURL(v.Unrendered, row.User, value); err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
		}
		c, err := SetVariable(s, pending, u.Loc.Place, u.Loc.Name, next, now)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		auto(&c)
		add(c, true)
	}
	return out, nil
}

// rotateProducer is the Origin.Producer of the changes Rotate makes.
const rotateProducer = "rotate"

// automatic reports whether c is a change a password decision made by
// itself (rotation.Automatic).
func automatic(c changeset.Change) bool { return rotation.Automatic(c) }

// pendingAt is the pending change at a place's variable.
func pendingAt(pending []changeset.Change, l rotation.Loc) (changeset.Change, bool) {
	return rotation.PendingAt(pending, l)
}

// PasswordFollowers are, when c is a database password change (db.password
// or db.keyfile), the ids of the other unlocked changes of its group the
// decision made by itself (removed with it), and how many decided places
// stay. Otherwise none.
func PasswordFollowers(pending []changeset.Change, c changeset.Change) (auto []string, decided int) {
	if c.Kind != kindDBPassword && c.Kind != kindDBKeyFile {
		return nil, 0
	}
	if _, _, ok := rotation.ParseDBGroupKey(c.Group); !ok {
		return nil, 0
	}
	for _, o := range pending {
		if o.ID == c.ID || o.Group != c.Group {
			continue
		}
		if automatic(o) && o.RunID == "" {
			auto = append(auto, o.ID)
		} else {
			decided++
		}
	}
	return auto, decided
}

// placeLabel is "<env> · <service> <NAME>" (rotation.PlaceLabel).
func placeLabel(s *store.Store, p domain.Place, name string) string {
	return rotation.PlaceLabel(s, p, name)
}

// serviceLabel is "<env> · <service>" (rotation.ServiceLabel).
func serviceLabel(s *store.Store, p domain.Place) string { return rotation.ServiceLabel(s, p) }

// CheckPasswordGroups refuses a selection sel of the change set live,
// before it is frozen, holding a database password group that cannot run.
// Every selected change of a password group must have
// what it Requires in sel (else it follows a removed password change, or its
// password change is left out). For each group whose db.password or
// db.keyfile change is selected, it refuses when: the group's row (read from
// s now) is gone or has a Problem; the database's environment (its owner,
// own variable, KEYFILE members) is not writable; a variable.set of a
// password group does not Require the password change; a root's own
// variable is not set to the new password (edited after the decision, or
// left out); a user row's URLs hold different passwords and one of them is
// not decided by hand; a rewritten place (or literal copy, or unrecognised
// place) is neither kept nor writable, or has no decision in sel; a KEYFILE
// member's redeploy is switched off; a redeploy action serving the group has
// no deployment (only when s has read the environment's deployments; the
// pre-flight reads Railway). A revert (SourceRevert) is checked for its row
// and environments only: its places already hold the value it sets.
// actions is nil when sel did not compile: the checks that need no actions
// still run, so the refusal names the reason. It reads only s, writeEnvs,
// the sets and actions.
func CheckPasswordGroups(s *store.Store, writeEnvs []string, sel, live changeset.ChangeSet, actions []apply.FrozenAction) error {
	if s == nil {
		return nil
	}
	o := rotation.Options{AllEnvIDs: allEnvIDs(s)}
	o.EnvIDs = o.AllEnvIDs
	selected := map[string]bool{}
	for _, c := range sel.Changes {
		selected[c.ID] = true
	}
	groups := map[string][]changeset.Change{}
	var order []string
	for _, c := range sel.Changes {
		if _, _, ok := rotation.ParseDBGroupKey(c.Group); !ok {
			continue
		}
		for _, r := range c.Requires {
			if selected[r] {
				continue
			}
			if _, ok := live.Get(r); ok {
				return errors.New(placeLabel(s, c.Place, c.Target) + " needs its password change in this run · select the whole group")
			}
			return errors.New(placeLabel(s, c.Place, c.Target) + " follows a removed password change · decide again or remove it")
		}
		if _, seen := groups[c.Group]; !seen {
			order = append(order, c.Group)
		}
		groups[c.Group] = append(groups[c.Group], c)
	}
	for _, g := range order {
		if err := checkPasswordGroup(s, writeEnvs, sel, actions, g, groups[g], o); err != nil {
			return err
		}
	}
	return nil
}

func checkPasswordGroup(s *store.Store, writeEnvs []string, sel changeset.ChangeSet, actions []apply.FrozenAction, group string, changes []changeset.Change, o rotation.Options) error {
	i := slices.IndexFunc(changes, func(c changeset.Change) bool { return c.Kind == kindDBPassword || c.Kind == kindDBKeyFile })
	if i < 0 {
		return nil // no password changes in this run
	}
	main := changes[i]
	row, ok := rotation.RowOf(s, group, o)
	if !ok {
		return errors.New("the database of a password in this run is gone · refresh")
	}
	if row.Problem != "" {
		return errors.New(text.Sanitize(row.Problem))
	}
	if msg := rotation.DBRefusal(s, row, func(env string) bool { return slices.Contains(writeEnvs, env) }); msg != "" {
		return errors.New(msg)
	}
	if main.Kind == kindDBPassword {
		nv, err := decodeDBPassword(main)
		if err != nil {
			return err
		}
		if nv.Source == SourceRevert {
			// A revert sets the database back to the value its places
			// already hold: no place needs a decision.
			return nil
		}
		for _, c := range changes {
			if c.Kind == kindSet && !slices.Contains(c.Requires, main.ID) {
				return errors.New(placeLabel(s, c.Place, c.Target) + " is not tied to the password change · decide it again")
			}
		}
		if row.Root && row.Own != nil && !ownSetsPassword(changes, *row.Own, dbFingerprint(main, nv)) {
			// Edited in Variables after the decision (it keeps the tie), or
			// left out: the database would get a password it does not hold.
			return errors.New(placeLabel(s, row.Own.Place, row.Own.Name) + " no longer matches the new password · decide again")
		}
	}
	byKey := map[string]changeset.Change{}
	for _, c := range changes {
		byKey[c.Key()] = c
	}
	for _, c := range sel.Changes {
		if c.Kind == kindKeep {
			byKey[c.Key()] = c
		}
	}
	_, mixed := rotation.CurrentPassword(s, row)
	mixed = mixed && !row.Root && !row.KeyFile
	for _, u := range slices.Concat(row.URLs, row.Copies) {
		if u.Mode != rotation.URLRewrite && u.Mode != rotation.URLUnrecognised {
			continue
		}
		c, has := byKey[u.Loc.Place.Key()+"|"+u.Loc.Name]
		switch {
		case mixed && u.Mode == rotation.URLRewrite && (!has || automatic(c)):
			// No single current password: every URL is decided by hand.
			return errors.New("the user's URLs hold different passwords · decide each place")
		case has && c.Kind == kindKeep:
		case !slices.Contains(writeEnvs, u.Loc.Place.EnvID):
			return errors.New(serviceLabel(s, u.Loc.Place) + " is read only · Settings w or k keeps it")
		case !has:
			return errors.New(placeLabel(s, u.Loc.Place, u.Loc.Name) + " has no decision · select it or k")
		}
	}
	if main.Kind == kindDBKeyFile {
		info, err := decodeKeyFile(main)
		if err != nil {
			return err
		}
		for _, p := range slices.Concat([]domain.Place{main.Place}, info.Members) {
			if slices.Contains(sel.RedeployOff, p.Key()) {
				return errors.New("turn the redeploy of " + serviceLabel(s, p) + " back on: a KEYFILE needs every member restarted")
			}
		}
	}
	ids := map[string]bool{}
	for _, c := range changes {
		ids[c.ID] = true
	}
	for _, a := range groupRedeploys(actions, ids) {
		if !s.StatesRead(a.EnvID) {
			continue
		}
		if st, ok := s.State(a.EnvID, a.ServiceID); !ok || st.ActiveID == "" && st.LatestStatus == "" && len(st.InProgress) == 0 {
			return errors.New(serviceLabel(s, a) + " has no deployment to redeploy")
		}
	}
	return nil
}

// ownSetsPassword reports whether changes hold a variable.set of the root's
// own variable own whose value has the fingerprint fp (dbFingerprint of the
// password change): the variable gets the password the database gets.
func ownSetsPassword(changes []changeset.Change, own rotation.Loc, fp string) bool {
	return slices.ContainsFunc(changes, func(c changeset.Change) bool {
		return c.Kind == kindSet && c.Place.Key() == own.Place.Key() && c.Target == own.Name &&
			(variableKind{}).NewFingerprint(c) == fp
	})
}

// groupRedeploys are the places of the redeploy actions serving any of ids.
func groupRedeploys(actions []apply.FrozenAction, ids map[string]bool) []domain.Place {
	var out []domain.Place
	for _, a := range actions {
		if strings.HasPrefix(a.Key, "redeploy:") && slices.ContainsFunc(a.Serves, func(id string) bool { return ids[id] }) {
			out = append(out, a.Place)
		}
	}
	return out
}

// allEnvIDs is every environment the store knows.
func allEnvIDs(s *store.Store) []string {
	var out []string
	for _, p := range s.Projects() {
		for _, e := range p.Environments {
			out = append(out, e.ID)
		}
	}
	return out
}
