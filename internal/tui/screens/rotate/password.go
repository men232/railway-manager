package rotate

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/view"
)

// Database password rows: one row per password
// with every place in every environment that carries it. The password line
// takes g, p and o; a rewritten URL line p, f, c, x, u and k; a reference
// follows by itself; other users' and unrecognised URLs are shown only.

// pwLineID is the location id of a user row's password line: the user's
// password lives only inside the database, so the line has no variable.
const pwLineID = "@password"

const (
	// stalledText refuses every decision on a stalled row (continue and
	// revert aside).
	stalledText = "continue or revert the stalled run first"
	// elsewhereText refuses a row whose run executes in another app
	// instance.
	elsewhereText = "running in another window"
	// handWarning is shown wherever a password changed by hand is.
	handWarning = "database not touched: Railway is written as if the password already changed inside it"
	// reconnectWarning is a password row's standing warning.
	reconnectWarning = "apps reconnect after redeploy · new connections fail ~1–2 min"
	// keyFilePrompt asks before a KEYFILE is decided.
	keyFilePrompt = "new KEYFILE? · every replica-set member restarts at once"
	// leakedValue refuses a URL value carrying the current password.
	leakedValue = "that is the current, leaked value"
	// pwLen is the length of a generated password (letters and digits).
	pwLen = 32
)

func pwRow(r view.RotateRow) bool { return r.Password != nil }

// pwLines is a password row's lines as the places screen lists them: the
// password line first (the own variable, or a line of its own for a user
// row), then every place.
func pwLines(r view.RotateRow) []view.RotatePlace {
	if r.Password.Own != nil {
		return r.Places
	}
	pw := view.RotatePlace{Own: true, Letter: -1, Loc: rotation.Loc{ID: pwLineID, Place: r.Password.DB.Owner}}
	return append([]view.RotatePlace{pw}, r.Places...)
}

// items is the lines the places screen lists for row r.
func items(r view.RotateRow) []view.RotatePlace {
	if pwRow(r) {
		return pwLines(r)
	}
	return r.Places
}

// shownOnly reports whether line p is another user's URL or another
// database's same value: shown dimmed, not counted, never under the cursor.
func shownOnly(r view.RotateRow, p view.RotatePlace) bool {
	return pwRow(r) && !p.Own && (p.URLMode == rotation.URLOther || p.URLMode == rotation.URLOtherDB)
}

// writable reports whether an environment is a write environment.
func writable(ctx *tui.Ctx) func(string) bool {
	return func(env string) bool { return ctx.Settings != nil && slices.Contains(ctx.Settings.WriteEnvs, env) }
}

// tie makes a set at a place of a password row wait for the row's password
// change, when one is pending (a KEYFILE's writes wait for nothing).
func tie(r view.RotateRow, c *changeset.Change) {
	if pwRow(r) && r.Change != nil && r.Change.Kind == "db.password" && c.Kind == "variable.set" {
		c.Requires = []string{r.Change.ID}
	}
}

// pwRowRefusal is why password row r takes no decision at all; nil when it
// can take one.
func pwRowRefusal(r view.RotateRow) tea.Cmd {
	switch {
	case r.Problem != "":
		return tui.Flash('!', text.Sanitize(r.Problem))
	case r.Stalled:
		return tui.Flash('✗', stalledText)
	case r.Run != nil && r.Run.Elsewhere:
		return tui.Flash('✗', elsewhereText)
	case r.Change != nil && r.Change.RunID != "":
		return tui.Flash('✗', "in run "+r.Change.RunID)
	case r.Run != nil && r.Run.Applying:
		return tui.Flash('✗', "in run "+r.Run.ID)
	}
	return nil
}

// pwLineRefusal is why line p of password row r cannot take action (a
// places key: g p o f c x k u); nil when it can. k is the only decision a
// read-only environment takes.
func pwLineRefusal(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace, action string) tea.Cmd {
	if cmd := pwRowRefusal(r); cmd != nil {
		return cmd
	}
	kf := r.Password.KeyFile
	if p.Own {
		switch {
		case action == "u":
			return nil
		case action == "o" && kf:
			return tui.Flash('!', "a KEYFILE is not changed by hand · g or p")
		case action == "x":
			return tui.Flash('✗', "the password's own variable is never deleted")
		case action != "g" && action != "p" && action != "o":
			return tui.Flash('!', "the password line takes g, p or o")
		}
		if msg := rotation.DBRefusal(ctx.Store, *r.Password, writable(ctx)); msg != "" {
			return tui.Flash('✗', msg)
		}
		return nil
	}
	label := rotation.PlaceLabel(ctx.Store, p.Loc.Place, p.Loc.Name)
	switch {
	case p.Pending != nil && p.Pending.RunID != "":
		return tui.Flash('✗', "in run "+p.Pending.RunID)
	case shownOnly(r, p):
		return tui.Flash('!', label+" holds another password · untouched")
	case p.URLMode == rotation.URLFollows:
		return tui.Flash('!', label+" follows by reference · decide it at its source")
	case kf:
		return tui.Flash('!', "a KEYFILE copy gets the new KEYFILE · decide the KEYFILE line")
	case (action == "g" || action == "o") && p.URLMode == rotation.URLUnrecognised:
		return tui.Flash('!', label+" carries this password · p f c x or k")
	case action == "g" || action == "o":
		return tui.Flash('!', action+" is for the password line · this URL gets the new password by itself")
	case action == "u":
		return nil
	case valueRotation(p.Pending):
		return tui.Flash('✗', label+" has a pending rotation change · u removes it first")
	case action == "k":
		return nil
	case p.ReadOnly:
		return tui.Flash('✗', rotation.ServiceLabel(ctx.Store, p.Loc.Place)+" is read only · k keeps it")
	}
	return nil
}

// valueRotation reports whether c is a change of a value rotation group.
func valueRotation(c *changeset.Change) bool {
	if c == nil {
		return false
	}
	_, ok := rotation.ParseGroupKey(c.Group)
	return ok
}

// passwordValue is the password g and space give row r: the one generated
// for it and not decided yet (a second key before the root has put the
// first), else a new one of 32 letters and digits (a KEYFILE as long as
// the current one, at most 1024).
func (pl *planner) passwordValue(ctx *tui.Ctx, r view.RotateRow) (string, error) {
	k := "pw:" + r.Key
	// A value kept for a second press before the root has put the first
	// is reused; once its change was seen pending (seen) or Railway holds
	// it, it would decide nothing new.
	if r.Change != nil {
		delete(pl.planned, k)
	}
	if v, ok := pl.planned[k]; ok && !rotation.HoldsPassword(v, current(ctx, r)) {
		return v, nil
	}
	n := pwLen
	if r.Password.KeyFile && r.Password.Own != nil {
		if v := ctx.Store.Variable(r.Password.Own.ID); v != nil {
			n = min(max(n, len(v.Unrendered)), kinds.KeyFileMax)
		}
	}
	v, err := rotation.Generate(n, false)
	if err == nil {
		pl.planned[k] = v
	}
	return v, err
}

// decidePassword puts every change of row r's password decision
// (kinds.NewPasswordChanges): source is a kinds.Source constant. A typed
// value equal to the password Railway holds now is refused.
func decidePassword(ctx *tui.Ctx, r view.RotateRow, source, value string) tea.Cmd {
	// A revert sets the current value back by design; it is never made
	// here (kinds.DBPasswordRevert).
	if source != kinds.SourceRevert && rotation.HoldsPassword(value, current(ctx, r)) {
		return tui.Flash('✗', "that is the current, leaked password")
	}
	if source != kinds.SourceGenerate {
		if r.Password.KeyFile {
			if msg := keyFileRefusal(value); msg != "" {
				return tui.Flash('✗', msg)
			}
		}
	}
	puts, err := kinds.NewPasswordChanges(ctx.Store, pending(ctx), *r.Password, source, value, ctx.Clock()())
	if err != nil {
		return tui.Flash('✗', err.Error())
	}
	label := text.Sanitize(r.Label)
	switch {
	case r.Password.KeyFile:
		return changes(nil, puts, '✓', label+": new KEYFILE")
	case source == kinds.SourceHand:
		return changes(nil, puts, '!', label+": changed by hand · database not touched")
	}
	return changes(nil, puts, '✓', label+": new password")
}

// current is the hashes of row r's current password; none for a user row
// whose URLs hold different passwords (rotation.CurrentPassword).
func current(ctx *tui.Ctx, r view.RotateRow) map[string]bool {
	h, _ := rotation.CurrentPassword(ctx.Store, *r.Password)
	return h
}

// keyFileRefusal is why a typed KEYFILE cannot be used
// (kinds.CheckKeyFile); "" when it can. Never holds the value.
func keyFileRefusal(v string) string {
	if err := kinds.CheckKeyFile(v); err != nil {
		return err.Error()
	}
	return ""
}

// seen forgets the values kept for password rows whose change is pending:
// the decision was put, so the next one is new.
func (pl *planner) seen(rows []view.RotateRow) {
	for _, r := range rows {
		if pwRow(r) && r.Change != nil {
			delete(pl.planned, "pw:"+r.Key)
		}
	}
}

// undoPassword removes row r's password change alone: the root removes its
// automatic rewrites with it, and the places decided by hand stay.
func (pl *planner) undoPassword(r view.RotateRow) tea.Cmd {
	if r.Change == nil {
		return tui.Flash('!', "no password decided yet")
	}
	if r.Change.RunID != "" {
		return tui.Flash('✗', "in run "+r.Change.RunID)
	}
	delete(pl.planned, "pw:"+r.Key)
	return changes([]string{r.Change.ID}, nil, '✓', text.Sanitize(r.Label)+": password decision undone")
}

// rebuild is row r's password decision made again without the changes
// gone, so the places they decided are rewritten with the password again;
// none when no password is decided. An error when the decision cannot be
// made again (the undo would leave a place undecided).
func rebuild(ctx *tui.Ctx, r view.RotateRow, gone map[string]bool) ([]changeset.Change, error) {
	if r.Change == nil || ctx.CS == nil {
		return nil, nil
	}
	full, ok := ctx.CS.Get(r.Change.ID)
	if !ok {
		return nil, nil
	}
	source, value, ok := kinds.PasswordValue(full)
	if !ok {
		return nil, errors.New("the password decision cannot be read · decide the password again")
	}
	rest := slices.DeleteFunc(slices.Clone(pending(ctx)), func(c changeset.Change) bool { return gone[c.ID] })
	return kinds.NewPasswordChanges(ctx.Store, rest, *r.Password, source, value, ctx.Clock()())
}

// fullChange is the pending change c as the change set holds it, values
// included; c itself when the set has no such change.
func fullChange(ctx *tui.Ctx, c *changeset.Change) changeset.Change {
	if ctx.CS != nil {
		if full, ok := ctx.CS.Get(c.ID); ok {
			return full
		}
	}
	return *c
}

// overlay is the kind's overlay of pending change c, from its full form.
func overlay(ctx *tui.Ctx, c *changeset.Change) apply.Overlay {
	full := fullChange(ctx, c)
	if k, ok := ctx.Views.Reg[full.Kind]; ok {
		return k.Overlay(full)
	}
	return apply.Overlay{}
}

// handSource reports whether row r's password change says the password was
// changed by hand already.
func handSource(ctx *tui.Ctx, r view.RotateRow) bool {
	if r.Change == nil {
		return false
	}
	src, _, ok := kinds.PasswordValue(fullChange(ctx, r.Change))
	return ok && src == kinds.SourceHand
}

func toneOf(th tui.Theme, tone string) lipgloss.Style {
	switch tone {
	case "danger":
		return th.Danger
	case "warn":
		return th.Warn
	}
	return th.New
}

// pwTag is a password row's icon and mode tag.
func pwTag(r view.RotateRow) string {
	switch {
	case r.Password.KeyFile:
		return "↻ KEY"
	case r.Password.Root:
		return "↻ DB "
	}
	return "⇄ USR"
}

// pwTagStyle is a password row's tag colour.
func pwTagStyle(ctx *tui.Ctx, r view.RotateRow) lipgloss.Style {
	switch {
	case r.Password.KeyFile:
		return ctx.Theme.Tags.Key
	case r.Password.Root:
		return ctx.Theme.Tags.DB
	}
	return ctx.Theme.Tags.User
}

// pwState is a password row's state column: stalled, applying, its
// decision, then what its last run did.
func pwState(ctx *tui.Ctx, r view.RotateRow) (string, lipgloss.Style) {
	th, ic := ctx.Theme, ctx.Icons
	frac := strconv.Itoa(r.Decided) + "/" + strconv.Itoa(r.Decidable)
	switch {
	case r.Problem != "":
		return ic.Fail() + " cannot rotate", th.Danger
	case r.Stalled:
		return ic.Fail() + " stalled", th.Danger
	case r.Run != nil && r.Run.Applying:
		return ic.Run() + " applying", th.Warn
	case r.Run != nil && r.Run.Elsewhere:
		return ic.Run() + " another window", th.Warn
	case r.Decided > 0 && r.Blocked != "":
		return ic.Fail() + " blocked", th.Danger
	case r.Decided > 0 && r.Decided == r.Decidable:
		return ic.Ok() + " " + frac, th.OK
	case r.Decided > 0:
		return ic.Status("◐") + " " + frac, th.Warn
	case r.Changed && r.Run.NotRedeployed > 0:
		return ic.Status("◐") + " changed · " + strconv.Itoa(r.Run.NotRedeployed) + " not redeployed", th.Warn
	case r.Changed:
		return ic.Ok() + " changed", th.OK
	}
	return "", th.Muted
}

// ownCount is how many own variables a password row has: its root
// password, or its KEYFILE and literal copies; 0 for a user row.
func ownCount(r view.RotateRow) int {
	n := 0
	for _, p := range r.Places {
		if p.Own || r.Password.KeyFile && p.URLMode == rotation.URLRewrite {
			n++
		}
	}
	return n
}

// urlCounts are the URL places of a password row by what happens to them.
type urlCounts struct {
	total, rewritten, yours, kept, left int
	// others are other users' URLs.
	others int
}

func countURLs(r view.RotateRow) urlCounts {
	var n urlCounts
	if r.Password.KeyFile {
		return n
	}
	for _, p := range r.Places {
		switch {
		case p.Own:
			continue
		case p.URLMode == rotation.URLOther:
			n.others++
			continue
		case p.URLMode == rotation.URLOtherDB:
			continue
		}
		n.total++
		switch {
		case p.URLMode == rotation.URLFollows:
			n.rewritten++
		case p.Kept:
			n.kept++
		case p.Pending != nil && p.Pending.Group == r.Key && !p.Auto:
			n.yours++
		case p.Pending != nil && p.Pending.Group != r.Key,
			p.Pending == nil && (p.Unclear || p.ReadOnly || p.URLMode == rotation.URLUnrecognised),
			p.Auto && p.ReadOnly:
			n.left++
		default:
			n.rewritten++
		}
	}
	return n
}

// urlsText is "N URLs: a rewritten · b your value · c kept · d left".
func urlsText(n urlCounts) string {
	parts := []string{strconv.Itoa(n.rewritten) + " rewritten", strconv.Itoa(n.yours) + " your value"}
	if n.kept > 0 {
		parts = append(parts, strconv.Itoa(n.kept)+" kept")
	}
	parts = append(parts, strconv.Itoa(n.left)+" left")
	return "URLs " + strconv.Itoa(n.total) + ":  " + strings.Join(parts, " · ")
}

// pwAction is the panel's first line after the tag and label: the decided
// password, or what space would decide.
func pwAction(ctx *tui.Ctx, r view.RotateRow) string {
	if r.Change != nil {
		return text.Sanitize(overlay(ctx, r.Change).Short)
	}
	if r.Password.KeyFile {
		return "new KEYFILE, letters+digits · members redeploy"
	}
	return "new password " + strconv.Itoa(pwLen) + " letters+digits"
}

// pwWhere is the panel's password line: where the password lives.
func pwWhere(ctx *tui.Ctx, r view.RotateRow) string {
	pr := r.Password
	switch {
	case pr.KeyFile && pr.Own != nil:
		n := ownCount(r) - 1
		return "keyfile   " + placeLabel(ctx, pr.Own.Place) + "   " + text.Sanitize(pr.Own.Name) + " · " + strconv.Itoa(n) + " copies"
	case pr.Own != nil:
		return "password  " + placeLabel(ctx, pr.Own.Place) + " · " + text.Sanitize(kindName(pr.DB.Kind)) + "   " + text.Sanitize(pr.Own.Name)
	}
	return "password  user " + text.Sanitize(pr.User) + " inside " + placeLabel(ctx, pr.DB.Owner) + " · " + text.Sanitize(kindName(pr.DB.Kind))
}

func kindName(kind string) string {
	switch kind {
	case "mongo":
		return "Mongo"
	case "postgres":
		return "Postgres"
	case "redis":
		return "Redis"
	}
	return kind
}

// pwNotes are a password row's standing lines in the panel and the places
// window: problem, stalled (cont says how to continue it), why it cannot
// run, the hand warning, warnings.
func pwNotes(ctx *tui.Ctx, r view.RotateRow, cont string) []string {
	th, ic := ctx.Theme, ctx.Icons
	var out []string
	if r.Problem != "" {
		out = append(out, th.Danger.Render(ic.Fail()+" "+text.Sanitize(r.Problem)))
	}
	if r.Stalled && r.Run != nil {
		l := ic.Fail() + " stalled: password changed"
		if r.Run.NotRedeployed > 0 {
			units := " apps not redeployed"
			if r.Password.KeyFile {
				units = " nodes not redeployed"
			}
			l = ic.Fail() + " stalled: " + strconv.Itoa(r.Run.NotRedeployed) + " of " + strconv.Itoa(r.Run.Redeploys) + units
		}
		out = append(out, th.Danger.Render(l+" · "+cont))
		if r.Revert {
			out = append(out, th.Warn.Render(ic.Warn()+" R reverts the database password: no place holds the new one"))
		}
	}
	if r.Run != nil && r.Run.Elsewhere {
		out = append(out, th.Warn.Render(ic.Run()+" "+elsewhereText))
	}
	if r.Blocked != "" {
		out = append(out, th.Danger.Render(ic.Fail()+" "+text.Sanitize(r.Blocked)))
	}
	if handSource(ctx, r) {
		out = append(out, th.Danger.Render(ic.Warn()+" "+handWarning))
	}
	if r.Warning != "" {
		out = append(out, th.Warn.Render(ic.Warn()+" "+text.Sanitize(r.Warning)))
	} else if !r.Password.KeyFile {
		out = append(out, th.Warn.Render(ic.Warn()+" "+reconnectWarning))
	}
	return out
}

// pwCols are the column widths of a password row's lines.
type pwCols struct {
	locCols
	nameW int
}

// pwColsOf fits the place and name columns to the lines shown, leaving the
// decision its width when it can: on a narrow screen what happens to a
// place matters more than its full name.
func pwColsOf(ctx *tui.Ctx, r view.RotateRow, show []view.RotatePlace, width int) pwCols {
	c := pwCols{locCols: locCols{short: shortEnvs(ctx)}}
	decW := 0
	for _, p := range show {
		c.fit(ctx, p)
		c.nameW = max(c.nameW, ansi.StringWidth(pwName(r, p)))
		d := pwOtherText(ctx, r, p)
		if !shownOnly(r, p) {
			d = pwDecision(ctx, r, p)
		}
		decW = max(decW, ansi.StringWidth(d))
	}
	c.limit(width)
	c.nameW = min(c.nameW, max(8, width/4))
	const minLoc = 12
	minName := 12
	if width < 70 { // a narrow screen keeps the decision readable
		minName = 8
	}
	// The name gives way first, then the decision down to maxDec cells (a
	// long one is cut sooner than the place it is about), then the place.
	const maxDec = 32
	room := width - 4 - decW
	if c.width()+c.nameW > room {
		c.nameW = max(minName, min(c.nameW, room-c.width()))
	}
	if c.width()+c.nameW > room {
		room = max(room, width-4-min(decW, maxDec))
		c.shrink(max(minLoc, room-c.nameW))
	}
	return c
}

// pwName is a line's name column: the variable, or the user of a user row's
// password line.
func pwName(r view.RotateRow, p view.RotatePlace) string {
	if p.Loc.ID == pwLineID {
		return "user " + text.Sanitize(r.Password.User)
	}
	return text.Sanitize(p.Loc.Name)
}

// pwPlaceLine is a password row's line: place, name, what happens to it.
// A shown-only line is dimmed as a whole.
func pwPlaceLine(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace, c pwCols) string {
	l := c.cells(ctx, p) + "  " + tui.Pad(pwName(r, p), c.nameW) + "  "
	if shownOnly(r, p) {
		return ctx.Theme.Muted.Render(l + pwOtherText(ctx, r, p))
	}
	return l + pwDecision(ctx, r, p)
}

// pwOtherText is a shown-only line's text.
func pwOtherText(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace) string {
	if p.URLMode == rotation.URLOtherDB {
		if db := r.Password.DB; p.Loc.Place.Key() == db.Owner.Key() && p.Loc.Name == rotation.RootVar(db.Kind) {
			return "same value · root of this database · untouched"
		}
		return "same value · other database · untouched"
	}
	if u := otherUser(ctx, r, p); u != "" {
		return "user '" + u + "' · untouched"
	}
	return "another user · untouched"
}

// otherUser is the user of another user's URL line p: the user of the row
// of the same database that rewrites it; "" when none does.
func otherUser(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace) string {
	for _, q := range rowsOf(ctx) {
		if !pwRow(q) || q.Key == r.Key || q.Password.DB.Key != r.Password.DB.Key || q.Password.KeyFile {
			continue
		}
		for _, l := range q.Places {
			if l.Loc.ID == p.Loc.ID && l.URLMode == rotation.URLRewrite {
				return text.Sanitize(q.Password.User)
			}
		}
	}
	return ""
}

// pwDecision is what happens to line p of password row r when it runs.
func pwDecision(ctx *tui.Ctx, r view.RotateRow, p view.RotatePlace) string {
	th := ctx.Theme
	if p.Own {
		switch {
		case r.Change != nil && r.Change.RunID != "":
			return th.Warn.Render(ctx.Icons.Run() + " " + text.Sanitize(r.Change.RunID))
		case r.Change != nil:
			ov := overlay(ctx, r.Change)
			return toneOf(th, ov.Tone).Render(text.Sanitize(ov.Short))
		case p.ReadOnly:
			return th.Muted.Render("read only")
		}
		return ""
	}
	if p.Pending != nil && p.Pending.RunID != "" {
		return th.Warn.Render(ctx.Icons.Run() + " " + text.Sanitize(p.Pending.RunID))
	}
	if p.URLMode == rotation.URLFollows {
		return th.Muted.Render("${{…}} follows")
	}
	if r.Password.KeyFile {
		if r.Change != nil {
			return th.New.Render("⇄ same KEYFILE")
		}
		return th.Muted.Render("⇄ same KEYFILE")
	}
	via := ""
	if p.ViaProxy {
		via = " (via proxy)"
		if r.Password.Root {
			via = " (root, via proxy)"
		}
	}
	var out string
	switch {
	case p.Kept:
		out = th.Danger.Render("old password stops working")
	case p.Auto && p.ReadOnly:
		out = th.Warn.Render("read only · k keeps it")
	case p.Auto && p.Copy:
		out = th.New.Render("⇄ same password")
	case p.Auto:
		out = th.New.Render("⇄ rewritten" + via)
	case p.Pending != nil && p.Pending.Group == r.Key:
		out = yourValue(ctx, p)
	case p.Pending != nil:
		out = th.Warn.Render(text.Sanitize(p.Pending.Kind) + " pending elsewhere · decide this place")
		if ov := p.Overlay.Short; ov != "" {
			out = th.Warn.Render(text.Sanitize(ov) + " · decide this place")
		}
	case p.Unclear:
		out = th.Warn.Render("set by hand · p f c x or k")
	case p.ReadOnly:
		out = th.Warn.Render("read only · k keeps it")
	case p.URLMode == rotation.URLUnrecognised:
		out = th.Warn.Render("carries this password · p f c x or k")
	case p.Copy:
		out = th.Muted.Render("⇄ same password")
	default:
		out = th.Muted.Render("⇄ rewritten" + via)
	}
	if d := tui.DriftText(ctx.Icons, p.Drifted); d != "" {
		out += "  " + th.Warn.Render(d)
	}
	return out
}

// yourValue is a URL place's own decision: a copy names its source, a set
// its length (never the value), anything else its overlay.
func yourValue(ctx *tui.Ctx, p view.RotatePlace) string {
	th := ctx.Theme
	if src, ok := copyFrom(ctx, p); ok {
		return th.New.Render("⇉ from " + placeLabel(ctx, src))
	}
	if v, ok := kinds.SetValue(fullChange(ctx, p.Pending)); ok {
		return th.New.Render(ctx.Icons.Edit() + " your value (" + strconv.Itoa(utf8.RuneCountInString(v)) + " chars)")
	}
	return toneOf(th, p.Overlay.Tone).Render(text.Sanitize(p.Overlay.Short))
}

// pwFooter is the places window's count line: "1 password · N URLs
// rewritten · M your value · K kept · L left · O other users".
func pwFooter(ctx *tui.Ctx, r view.RotateRow) (string, lipgloss.Style) {
	th := ctx.Theme
	if r.Password.KeyFile {
		n := ownCount(r) - 1
		if r.Change == nil {
			return "KEYFILE not decided · " + strconv.Itoa(n) + " copies", th.Muted
		}
		return "1 KEYFILE · " + strconv.Itoa(n) + " copies get it", th.OK
	}
	n := countURLs(r)
	pw, style, danger := "1 password", th.OK, false
	switch {
	case r.Change == nil:
		pw, style = "password not decided", th.Muted
	case handSource(ctx, r):
		pw, style, danger = "password changed by hand", th.Danger, true
	}
	urls := " URLs rewritten"
	if n.rewritten == 1 {
		urls = " URL rewritten"
	}
	parts := []string{pw, strconv.Itoa(n.rewritten) + urls, strconv.Itoa(n.yours) + " your value"}
	if n.kept > 0 {
		parts = append(parts, strconv.Itoa(n.kept)+" kept")
	}
	if n.left > 0 {
		parts = append(parts, strconv.Itoa(n.left)+" left")
		if !danger {
			style = th.Warn
		}
	}
	switch {
	case n.others == 1:
		parts = append(parts, "1 other user")
	case n.others > 1:
		parts = append(parts, strconv.Itoa(n.others)+" other users")
	}
	return strings.Join(parts, " · "), style
}
