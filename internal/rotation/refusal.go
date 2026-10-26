package rotation

import (
	"slices"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
)

// ServiceLabel is "<env> · <service>" ("shared" for the shared scope),
// sanitized.
func ServiceLabel(s *store.Store, p domain.Place) string {
	svc := "shared"
	if !p.Shared() {
		svc = s.ServiceName(p.ServiceID)
	}
	return text.Sanitize(s.EnvName(p.EnvID)) + " · " + text.Sanitize(svc)
}

// PlaceLabel is "<env> · <service> <NAME>", sanitized.
func PlaceLabel(s *store.Store, p domain.Place, name string) string {
	return ServiceLabel(s, p) + " " + text.Sanitize(name)
}

// DBRefusal is why a password row's database cannot be written, "" when it
// can: its owner, its own variable's place and, for a KEYFILE, its members
// must be in writable environments (write reports one).
func DBRefusal(s *store.Store, row PasswordRow, write func(envID string) bool) string {
	places := []domain.Place{row.DB.Owner}
	if row.Own != nil {
		places = append(places, row.Own.Place)
	}
	if row.KeyFile {
		places = append(places, row.DB.Members...)
	}
	for _, p := range places {
		if !write(p.EnvID) {
			return ServiceLabel(s, p) + " is read only · Settings w"
		}
	}
	return ""
}

// PlaceRefusal is why one place of a password row stops its decision or its
// run, "" when none. The place is the row's own variable (row.Own), a
// rewritten URL or literal copy (Mode URLRewrite) or an unrecognised place
// (URLUnrecognised, decided by hand like an Unclear URL); any other place
// never refuses. pending is the change set.
//
//   - a pending change at the place outside the row's group (an edit made
//     elsewhere, a value rotation, another group's keep) refuses: the
//     decision would silently replace it;
//   - a keep of the row's group: none;
//   - a place in an environment write refuses (unless kept); write nil
//     leaves writability to the run's check (a decision being made);
//   - the user's decision (a change of the group not made automatically):
//     none;
//   - an Unclear URL or an unrecognised place: set it by hand;
//   - decided (the row's password change is pending) and no change at a
//     rewritten place: it needs a decision.
//
// Labels are sanitized.
func PlaceRefusal(s *store.Store, row PasswordRow, p URLPlace, pending []changeset.Change, write func(envID string) bool, decided bool) string {
	own := row.Own != nil && p.Loc.ID == row.Own.ID
	if !own && p.Mode != URLRewrite && p.Mode != URLUnrecognised {
		return ""
	}
	label := PlaceLabel(s, p.Loc.Place, p.Loc.Name)
	c, has := PendingAt(pending, p.Loc)
	switch {
	case has && c.Group != row.Key && c.Group == "":
		return label + " has a pending edit · decide this place"
	case has && c.Group != row.Key:
		return label + " has a pending rotation change · remove it first"
	case has && c.Kind == keepKind && !own:
		return ""
	case write != nil && !write(p.Loc.Place.EnvID) && own:
		return ServiceLabel(s, p.Loc.Place) + " is read only · Settings w"
	case write != nil && !write(p.Loc.Place.EnvID):
		return ServiceLabel(s, p.Loc.Place) + " is read only · Settings w or k keeps it"
	case own:
		return ""
	case has && !Automatic(c):
		return ""
	case p.Mode == URLUnrecognised:
		return label + " carries this password · p f c x or k"
	case p.Unclear:
		return label + ": set this place by hand"
	case !has && decided:
		return label + " has no decision · select it or k"
	}
	return ""
}

// PendingAt is the pending change at a location's place and name.
func PendingAt(pending []changeset.Change, l Loc) (changeset.Change, bool) {
	key := l.Place.Key() + "|" + l.Name
	i := slices.IndexFunc(pending, func(c changeset.Change) bool { return c.Key() == key })
	if i < 0 {
		return changeset.Change{}, false
	}
	return pending[i], true
}

// Automatic reports whether c is a change a password decision made by
// itself (Origin.Letter changeset.LetterRewrite), not one the user decided.
func Automatic(c changeset.Change) bool {
	return c.Origin != nil && c.Origin.Letter == changeset.LetterRewrite
}
