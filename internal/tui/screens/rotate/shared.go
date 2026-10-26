package rotate

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/view"
)

// The helpers the list, the places screen and add share.

// rowsOf is every rotation row of the selected environments, hidden ones
// included.
func rowsOf(ctx *tui.Ctx) []view.RotateRow {
	if ctx.Views == nil || ctx.Store == nil {
		return nil
	}
	o := rotation.Options{EnvIDs: envIDs(ctx), AllEnvIDs: allEnvIDs(ctx), Since: since(ctx)}
	var write []string
	if ctx.Settings != nil {
		o.Internal = ctx.Settings.Internal
		write = ctx.Settings.WriteEnvs
	}
	return ctx.Views.Rotate(ctx.CS, o, write, tui.RotateRuns(ctx))
}

// rowByKey is the row with key, as its own key or one of its groups' (a
// refresh can reorder a row's groups and so change its key); false when
// no row has it now.
func rowByKey(ctx *tui.Ctx, key string) (view.RotateRow, bool) {
	rows := rowsOf(ctx)
	for _, r := range rows {
		if r.Key == key {
			return r, true
		}
	}
	for _, r := range rows {
		for _, g := range r.Groups {
			if g.Key == key {
				return r, true
			}
		}
	}
	return view.RotateRow{}, false
}

func pending(ctx *tui.Ctx) []changeset.Change {
	if ctx.CS == nil {
		return nil
	}
	return ctx.CS.Changes
}

// decidable reports whether a line of a decidable row can take a decision.
func decidable(p view.RotatePlace) bool { return !p.ReadOnly && !p.DB }

// open reports whether line p of row r still needs a decision: it can take
// one, has no change, and its group was not rotated everywhere after the
// leak (that line already holds a new value).
func open(r view.RotateRow, p view.RotatePlace) bool {
	return decidable(p) && p.Pending == nil && !rotatedGroup(r, p)
}

// openCount is how many lines of row r are open.
func openCount(r view.RotateRow) int {
	n := 0
	for _, p := range r.Places {
		if open(r, p) {
			n++
		}
	}
	return n
}

func isRotate(c *changeset.Change) bool {
	return c != nil && c.Origin != nil && c.Origin.Producer == producer
}

func placesText(n int) string {
	if n == 1 {
		return "1 place"
	}
	return strconv.Itoa(n) + " places"
}

// lineRefusal is why line p of row r cannot take a decision; nil when it
// can.
func lineRefusal(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace) tea.Cmd {
	switch {
	case pwRow(r):
		return pwLineRefusal(ctx, r, p, "")
	case r.Problem != "":
		return tui.Flash('!', r.Problem)
	case p.ReadOnly:
		return tui.Flash('✗', ctx.Store.EnvLabel(p.Loc.Place.EnvID)+" is not in write environments · Settings w")
	case p.DB:
		return tui.Flash('!', ctx.Store.ServiceName(p.Loc.Place.ServiceID)+" is a database · its password is in the database section")
	case p.Pending != nil && p.Pending.RunID != "":
		return tui.Flash('✗', "in run "+p.Pending.RunID)
	}
	return nil
}

// whyNot flashes why a row has no line to decide: its first line's reason.
func whyNot(ctx *tui.Ctx, r view.RotateRow) tea.Cmd {
	for _, p := range r.Places {
		if !decidable(p) {
			return lineRefusal(ctx, r, p)
		}
	}
	return nil
}

// groupIndex maps a row's location ids to the index of their group.
func groupIndex(r view.RotateRow) map[string]int {
	out := map[string]int{}
	for gi, g := range r.Groups {
		for _, l := range g.Locs {
			out[l.ID] = gi
		}
	}
	return out
}

// lineGroup is the group of line p, nil on an added row (no old value).
func lineGroup(r view.RotateRow, p view.RotatePlace) *rotation.Group {
	if r.Section == rotation.SectionAdded || len(r.Groups) == 0 {
		return nil
	}
	return &r.Groups[groupIndex(r)[p.Loc.ID]]
}

// mark gives a change Rotate makes at line p its group and origin.
func mark(c *changeset.Change, r view.RotateRow, p view.RotatePlace) {
	letter := ""
	if p.Letter >= 0 {
		letter = rotation.Letter(p.Letter)
	}
	if g := lineGroup(r, p); g != nil {
		c.Group = rotation.GroupKey(g.Key)
	} else {
		c.Group = r.Key // an added variable's group
	}
	c.Origin = &changeset.Origin{Producer: producer, Row: r.Key, Letter: letter}
}

// planKey is what shares one new value: the group, for a sealed line
// (Letter -1) the row and the name, for an added row the row.
func planKey(r view.RotateRow, p view.RotatePlace) string {
	switch g := lineGroup(r, p); {
	case g == nil:
		return "n:" + r.Key
	case p.Letter < 0:
		return "s:" + r.Key + "|" + p.Loc.Name
	default:
		return "g:" + g.Key
	}
}

// sharesValue reports whether pending change c carries the new value line
// p of row r gets: a change of its group, for a sealed line a Rotate change
// of the row at a sealed line of the same name, on an added row a set of
// the add group.
func sharesValue(r view.RotateRow, p view.RotatePlace, c *changeset.Change) bool {
	g := lineGroup(r, p)
	switch {
	case g == nil:
		return r.Section == rotation.SectionAdded && c.Group == r.Key && c.Kind == "variable.set"
	case p.Letter >= 0:
		return c.Group == rotation.GroupKey(g.Key)
	}
	_, ok := rotation.ParseGroupKey(c.Group)
	return ok && isRotate(c) && c.Origin.Row == r.Key && c.Origin.Letter == "" && c.Target == p.Loc.Name
}

// planner remembers the values a screen generated, by plan key, so a
// second generate before the root has put the first batch gives the same
// value.
type planner struct {
	planned map[string]string
}

func newPlanner() planner { return planner{planned: map[string]string{}} }

// newValue is the value line p gets: the value a pending change sharing it
// carries, else the value generated for it and not seen put yet, else a
// new one (24 letters and digits on an added row).
func (pl *planner) newValue(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace) (string, error) {
	for _, c := range pending(ctx) {
		if !sharesValue(r, p, &c) {
			continue
		}
		// A set edited elsewhere to hold a reference is no new value.
		if v, ok := kinds.SetValue(c); ok && v != "" && domain.IsLiteral(v) {
			return v, nil
		}
	}
	k := planKey(r, p)
	if v, ok := pl.planned[k]; ok {
		return v, nil
	}
	oldLen, hex := 0, false
	if g := lineGroup(r, p); g != nil {
		oldLen, hex = g.OldLen, g.Hex
	}
	v, err := rotation.Generate(oldLen, hex)
	if err == nil {
		pl.planned[k] = v
	}
	return v, err
}

// forget drops the planned values of lines whose value no pending change
// carries any more, gone (removed) excepted.
func (pl *planner) forget(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace, gone map[string]bool) {
	for _, p := range lines {
		used := false
		for _, c := range pending(ctx) {
			if !gone[c.ID] && sharesValue(r, p, &c) {
				used = true
				break
			}
		}
		if !used {
			delete(pl.planned, planKey(r, p))
		}
	}
}

// generate puts a variable.set with a new value at each line: one value
// per group, per name across the row's sealed lines, per added variable.
// Done is "✓ N places: new value".
func (pl *planner) generate(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace) tea.Cmd {
	return pl.generateDone(ctx, r, lines, '✓', placesText(len(lines))+": new value")
}

// generateDone is generate with its own Done text and kind: the root
// flashes it only when every put succeeds, else the first refusal.
func (pl *planner) generateDone(ctx *tui.Ctx, r view.RotateRow, lines []view.RotatePlace, kind rune, flash string) tea.Cmd {
	values := map[string]string{}
	puts := make([]changeset.Change, 0, len(lines))
	for _, p := range lines {
		k := planKey(r, p)
		v, ok := values[k]
		if !ok {
			var err error
			if v, err = pl.newValue(ctx, r, p); err != nil {
				return tui.Flash('✗', "cannot generate: "+err.Error())
			}
			values[k] = v
		}
		c, err := kinds.SetVariable(ctx.Store, pending(ctx), p.Loc.Place, p.Loc.Name, v, ctx.Clock()())
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		mark(&c, r, p)
		c.Origin.Source = changeset.SourceGenerate
		puts = append(puts, c)
	}
	return changes(nil, puts, kind, flash)
}

// changes sends a decision's removes and puts to the root as one
// tui.ChangesMsg: the root flashes flash (of kind) when every one is
// applied, else its first refusal, which a flash of the screen's own would
// hide.
func changes(removes []string, puts []changeset.Change, kind rune, flash string) tea.Cmd {
	return tui.Emit(tui.ChangesMsg{Remove: removes, Put: puts, Kind: kind, Done: text.Sanitize(flash)})
}

// leaked reports whether value is an old value of row r:
// its fingerprint, as the store computes it under any name of the row or
// of the target lines, equals a group's key. Groups changed everywhere
// after the leak are exempt. A long value's fingerprint is its hash alone;
// a short one's holds the name, hence every name.
func leaked(r view.RotateRow, lines []view.RotatePlace, value string) bool {
	names := map[string]bool{}
	for _, g := range r.Groups {
		for _, n := range g.Names {
			names[n] = true
		}
	}
	for _, p := range lines {
		names[p.Loc.Name] = true
	}
	lit := domain.IsLiteral(value)
	for _, g := range r.Groups {
		if g.AllFresh || g.Sealed {
			continue
		}
		for n := range names {
			if domain.Fingerprint(n, value, false, lit, "", "") == g.Key {
				return true
			}
		}
	}
	return false
}

// placeLabel is "<env> · <service>" for a place, sanitized.
func placeLabel(ctx *tui.Ctx, p domain.Place) string {
	return envText(shortEnvs(ctx), ctx.Store.EnvLabel(p.EnvID)) + " · " + text.Sanitize(ctx.Store.ServiceName(p.ServiceID))
}

// copySourceLoc is the location id a pending set at line p copies its
// value from; false when it is not a copy.
func copySourceLoc(ctx *tui.Ctx, p view.RotatePlace) (string, bool) {
	if p.Pending == nil || ctx.CS == nil {
		return "", false
	}
	full, ok := ctx.CS.Get(p.Pending.ID)
	if !ok {
		return "", false
	}
	return kinds.CopySource(full)
}

// copyFrom is the place a pending set at line p copies its value from;
// false when it is not a copy.
func copyFrom(ctx *tui.Ctx, p view.RotatePlace) (domain.Place, bool) {
	loc, ok := copySourceLoc(ctx, p)
	if !ok {
		return domain.Place{}, false
	}
	parts := strings.SplitN(loc, "/", 3)
	if len(parts) != 3 {
		return domain.Place{}, false
	}
	src := domain.Place{EnvID: parts[0]}
	if parts[1] != "shared" {
		src.ServiceID = parts[1]
	}
	return src, true
}

// maxFile is the largest file f reads.
const maxFile = 64 << 10

// secret is a value read from a file. It prints masked, so a message that
// carries it never shows it by accident.
type secret string

func (secret) String() string   { return "•••" }
func (secret) GoString() string { return "•••" }

// fileReadMsg is a file read's result for the reader seq: the trimmed
// content, or why it cannot be used.
type fileReadMsg struct {
	seq   int
	value secret
	err   string
}

// readFile reads path off the UI goroutine: at most maxFile bytes, trimmed.
// Errors never carry the content or the path.
func readFile(path string, seq int) tea.Cmd {
	return func() tea.Msg {
		v, err := readValue(path)
		if err != "" {
			return fileReadMsg{seq: seq, err: err}
		}
		return fileReadMsg{seq: seq, value: secret(v)}
	}
}

func readValue(path string) (string, string) {
	// Stat first, for a plain answer; the opened file is checked again.
	st, err := os.Stat(path)
	if err != nil {
		return "", "cannot read the file: " + reason(err)
	}
	if !st.Mode().IsRegular() {
		return "", "cannot read the file: not a regular file"
	}
	if st.Size() > maxFile {
		return "", "file too large"
	}
	f, err := openValue(path)
	if err != nil {
		return "", "cannot read the file: " + reason(err)
	}
	defer f.Close()
	if st, err = f.Stat(); err != nil {
		return "", "cannot read the file: " + reason(err)
	}
	if !st.Mode().IsRegular() {
		return "", "cannot read the file: not a regular file"
	}
	if st.Size() > maxFile {
		return "", "file too large"
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return "", "cannot read the file: " + reason(err)
	}
	defer clear(b)
	if len(b) > maxFile {
		return "", "file too large"
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", "the file is empty"
	}
	return v, ""
}

// openValue opens a value file for reading without blocking: the path may
// have become a FIFO since its Stat, and opening one would hang until a
// writer came. The caller still checks the opened file is regular.
func openValue(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// reason is an error without the path a PathError names.
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
