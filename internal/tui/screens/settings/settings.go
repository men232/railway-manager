// Package settings is the settings screen: which projects and environments
// are read, which may be written, the icon set and the load status.
package settings

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
)

// pickedMsg carries a picker's result back to the screen below it.
type pickedMsg struct {
	kind string // "projects", "envs" or "write"
	ids  []string
}

// sinceLayout is how the leak time is typed and shown, in local time.
const sinceLayout = "2006-01-02 15:04"

// screen holds the open text input, if any: "since" or "internal", and
// the cursor: the key of the selected setting.
type screen struct {
	field  *components.TextInput
	kind   string
	cursor string
}

// order is the settings the cursor stops at, as shown.
var order = []string{"p", "e", "w", "l", "i", "g", "a"}

// New returns the Settings screen.
func New() tui.Screen { return &screen{} }

func (*screen) Init(*tui.Ctx) tea.Cmd { return nil }
func (*screen) Title() string         { return "Settings" }

// Capturing reports whether an input takes every key.
func (s *screen) Capturing() bool { return s.field != nil }

func (s *screen) Keys() []tui.KeyHint {
	if s.field != nil {
		return []tui.KeyHint{
			{Key: "enter", Label: "confirm", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "↑↓", Label: "move", Rank: 1},
		{Key: "enter", Label: "change", Rank: 1},
		{Key: "p", Label: "projects", Rank: 2},
		{Key: "e", Label: "environments", Rank: 3},
		{Key: "w", Label: "write envs", Rank: 4},
		{Key: "l", Label: "leak time", Rank: 5, Optional: true},
		{Key: "i", Label: "internal names", Rank: 6, Optional: true},
		{Key: "g", Label: "icons", Rank: 7, Optional: true},
		{Key: "a", Label: "account", Rank: 8, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

func set(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// envItems lists the environments of the selected projects, or of every
// project when none is selected.
func envItems(ctx *tui.Ctx) []picker.Item {
	var items []picker.Item
	for _, p := range ctx.Store.Projects() {
		if len(ctx.Settings.Projects) > 0 && !slices.Contains(ctx.Settings.Projects, p.ID) {
			continue
		}
		for _, e := range p.Environments {
			items = append(items, picker.Item{ID: e.ID, Label: p.Name + "/" + e.Name})
		}
	}
	return items
}

// open asks for a picker. Saved ids missing from items are added as checked
// "(not in catalog)" items, so confirming never drops them silently. With no
// items at all it flashes instead.
func open(ctx *tui.Ctx, title string, items []picker.Item, selected []string, kind string) tea.Cmd {
	if len(items) == 0 {
		return tui.Emit(tui.FlashMsg{Kind: '!', Text: "nothing to pick yet · r refresh"})
	}
	known := make(map[string]bool, len(items))
	for _, it := range items {
		known[it.ID] = true
	}
	for _, id := range selected {
		if known[id] {
			continue
		}
		known[id] = true
		label := id + " (not in catalog)"
		if l := ctx.Store.EnvLabel(id); l != id {
			label = l + " (not selected)"
		}
		items = append(items, picker.Item{ID: id, Label: label})
	}
	done := func(ids []string) tea.Msg { return pickedMsg{kind: kind, ids: ids} }
	return tui.Emit(tui.OpenMsg{Name: "picker", Args: []any{title, items, set(selected), done}})
}

// sinceText is the leak time as typed: local time, or "" when unset.
func sinceText(since string) string {
	if since == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return text.Sanitize(since)
	}
	return t.In(time.Local).Format(sinceLayout)
}

// submit stores an input's answer and saves, or flashes why not.
func (s *screen) submit(ctx *tui.Ctx, v string) tea.Cmd {
	v = strings.TrimSpace(v)
	switch s.kind {
	case "since":
		if v == "" {
			ctx.Settings.Since = ""
			break
		}
		t, err := time.ParseInLocation(sinceLayout, v, time.Local)
		if err != nil {
			return tui.Flash('✗', "leak time is YYYY-MM-DD HH:MM")
		}
		ctx.Settings.Since = t.UTC().Format(time.RFC3339)
	case "internal":
		var names []string
		for _, n := range strings.Split(v, ",") {
			n = strings.ToUpper(strings.TrimSpace(n))
			if n == "" {
				continue
			}
			if kinds.CheckName(n) != nil {
				return tui.Flash('✗', n+" is not a variable name")
			}
			names = append(names, n)
		}
		slices.Sort(names)
		ctx.Settings.Internal = slices.Compact(names)
	}
	s.field, s.kind = nil, ""
	return tui.Emit(tui.SaveSettingsMsg{})
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if ctx.Settings == nil {
		return s, nil
	}
	switch msg := msg.(type) {
	case components.CancelMsg:
		s.field, s.kind = nil, ""
		return s, nil
	case components.SubmitMsg:
		if s.field == nil {
			return s, nil
		}
		return s, s.submit(ctx, msg.Value)
	case pickedMsg:
		switch msg.kind {
		case "projects":
			ctx.Settings.Projects = msg.ids
			return s, tea.Batch(tui.Emit(tui.SaveSettingsMsg{}), tui.Emit(tui.RefreshMsg{}))
		case "envs":
			ctx.Settings.Envs = msg.ids
			return s, tea.Batch(tui.Emit(tui.SaveSettingsMsg{}), tui.Emit(tui.RefreshMsg{}))
		case "write":
			ctx.Settings.WriteEnvs = msg.ids
			return s, tui.Emit(tui.SaveSettingsMsg{})
		}
	case tea.KeyMsg:
		if s.field != nil {
			return s, s.field.Update(msg)
		}
		key := ""
		switch msg.Type {
		case tea.KeyUp:
			s.move(-1)
		case tea.KeyDown, tea.KeyTab:
			s.move(1)
		case tea.KeyHome:
			s.move(-len(order))
		case tea.KeyEnd:
			s.move(len(order))
		case tea.KeyEnter:
			key = s.at()
		case tea.KeyRunes:
			switch key = string(msg.Runes); key {
			case "j":
				s.move(1)
			case "k":
				s.move(-1)
			}
		}
		if slices.Contains(order, key) {
			s.cursor = key
		}
		switch key {
		case "p":
			var items []picker.Item
			for _, p := range ctx.Store.Projects() {
				items = append(items, picker.Item{ID: p.ID, Label: p.Name, Note: p.Workspace})
			}
			return s, open(ctx, "Projects", items, ctx.Settings.Projects, "projects")
		case "e":
			return s, open(ctx, "Environments", envItems(ctx), ctx.Settings.Envs, "envs")
		case "w":
			return s, open(ctx, "Write environments", envItems(ctx), ctx.Settings.WriteEnvs, "write")
		case "l":
			s.field, s.kind = &components.TextInput{Label: "leak time (YYYY-MM-DD HH:MM, empty clears)"}, "since"
			s.field.SetValue(sinceText(ctx.Settings.Since))
			return s, nil
		case "i":
			s.field, s.kind = &components.TextInput{Label: "internal names (comma separated)"}, "internal"
			s.field.SetValue(strings.Join(ctx.Settings.Internal, ", "))
			return s, nil
		case "a":
			return s, tui.Flash('!', "railway login in a terminal signs in again")
		case "g":
			next := map[string]string{"auto": "nerd", "nerd": "plain", "plain": "auto"}[ctx.Settings.Icons]
			if next == "" {
				next = "nerd"
			}
			ctx.Settings.Icons = next
			switch next {
			case "nerd":
				ctx.Icons.Nerd = true
			case "plain":
				ctx.Icons.Nerd = false
			default:
				ctx.Icons.Nerd = ctx.NerdDetected
			}
			return s, tui.Emit(tui.SaveSettingsMsg{})
		}
	}
	return s, nil
}

// at is the key of the selected setting.
func (s *screen) at() string {
	if !slices.Contains(order, s.cursor) {
		return order[0]
	}
	return s.cursor
}

// move moves the cursor by d settings, staying within them.
func (s *screen) move(d int) {
	i := slices.Index(order, s.at())
	s.cursor = order[min(max(i+d, 0), len(order)-1)]
}

// setting is one entry of the list: its key, icon, label, the short value
// beside it, and the detail panel's text.
type setting struct {
	key, icon, label, short string
	about                   string   // what it means, wrapped in the panel
	value                   []string // the full value, already laid out
	hint                    string   // what enter does
}

// listW is the list column's width on a wide screen; a line and a space
// on each side of it part the list from the panel.
const listW = 28

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	if ctx.Settings == nil {
		return th.Warn.Render("settings not loaded")
	}
	ic := ctx.Icons
	wide := width >= 70
	detailW := width - listW - 3
	if !wide {
		detailW = width
	}
	items := s.settings(ctx, max(8, detailW))

	// the list: the settings, then the account
	byKey := map[string]setting{}
	for _, it := range items {
		byKey[it.key] = it
	}
	listInner := listW - 1
	if !wide {
		listInner = width
	}
	var list []string
	sel := 0 // the selected setting's line in list
	for _, k := range order {
		it := byKey[k]
		label := it.icon + ic.Gap() + it.label
		short := th.Muted.Render(it.short)
		gap := max(1, listInner-2-ansi.StringWidth(label)-ansi.StringWidth(it.short))
		if k == s.at() {
			sel = len(list)
			list = append(list, th.Accent.Render("▌ ")+th.Accent.Bold(true).Render(label)+strings.Repeat(" ", gap)+th.Accent.Render(it.short))
			continue
		}
		list = append(list, "  "+label+strings.Repeat(" ", gap)+short)
	}

	// the detail of the selected setting
	cur := byKey[s.at()]
	detail := []string{th.Accent.Bold(true).Render(cur.icon + ic.Gap() + cur.label), ""}
	for _, l := range wrapWords(cur.about, max(8, detailW)) {
		detail = append(detail, th.Muted.Render(l))
	}
	detail = append(detail, "")
	detail = append(detail, cur.value...)
	detail = append(detail, "", th.Accent.Render("enter")+" "+th.Muted.Render(cur.hint))

	var errs []string
	for _, id := range append([]string{"catalog"}, inventoryKeys(ctx.Settings.Projects)...) {
		if e := ctx.Store.Load(id).Err; e != "" {
			errs = append(errs, th.Danger.Render(ic.Fail()+ic.Gap()+text.Sanitize(e)))
		}
	}

	// scroll scrolls lines to fit h, the selected line in view.
	scroll := func(lines []string, h int) []string {
		if len(lines) <= h || h < 1 {
			return lines
		}
		from := min(max(0, sel-h+2), len(lines)-h)
		if sel-from < 1 {
			from = max(0, sel-1)
		}
		return lines[from : from+h]
	}
	// below puts the load failures at the bottom of lines, h tall, under
	// a thin rule.
	below := func(lines []string, h, w int) []string {
		if len(errs) == 0 {
			return lines
		}
		foot := append([]string{th.Faint.Render(strings.Repeat("─", w))}, errs...)
		for len(lines) < h-len(foot) {
			lines = append(lines, "")
		}
		return append(lines[:max(0, min(len(lines), h-len(foot)))], foot...)
	}
	var out []string
	if wide {
		right := below(detail, height, detailW)
		left := scroll(list, height)
		for i := range max(height, len(right)) {
			l, r := "", ""
			if i < len(left) {
				l = left[i]
			}
			if i < len(right) {
				r = right[i]
			}
			out = append(out, tui.Pad(ansi.Truncate(l, listW, "…"), listW)+" "+th.Faint.Render("│")+" "+ansi.Truncate(r, detailW, "…"))
		}
	} else {
		listH := min(len(list), max(3, height/2))
		out = scroll(list, listH)
		out = append(out, th.Faint.Render(strings.Repeat("─", width)))
		for _, l := range below(detail, height-listH-1, width) {
			out = append(out, ansi.Truncate(l, width, "…"))
		}
	}
	if s.field != nil && height > 0 {
		out = append(out[:max(0, min(len(out), height-1))], s.field.View(width))
	}
	if len(out) > height {
		out = out[:max(height, 0)]
	}
	return strings.Join(out, "\n")
}

// lastRead is when Railway was last read, or "never".
func (s *screen) lastRead(ctx *tui.Ctx) string {
	var newest time.Time
	for _, id := range append([]string{store.CatalogKey}, inventoryKeys(ctx.Settings.Projects)...) {
		if t := ctx.Store.Load(id).ReadAt; t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return "never"
	}
	return newest.Format("2006-01-02 15:04")
}

// settings are the entries with their values laid out w wide.
func (s *screen) settings(ctx *tui.Ctx, w int) []setting {
	th, ic, st := ctx.Theme, ctx.Icons, ctx.Settings
	projects := ctx.Store.Projects()
	// the projects under their workspace, each workspace named once
	var proj, missing []string
	byWS := map[string][]string{}
	var workspaces []string
	for _, id := range st.Projects {
		i := slices.IndexFunc(projects, func(p railway.Project) bool { return p.ID == id })
		if i < 0 {
			missing = append(missing, th.Warn.Render(ic.Project()+ic.Gap()+text.Sanitize(id)+" · not in the catalog"))
			continue
		}
		ws := text.Sanitize(projects[i].Workspace)
		if _, ok := byWS[ws]; !ok {
			workspaces = append(workspaces, ws)
		}
		byWS[ws] = append(byWS[ws], "  "+th.Accent2.Render(ic.Project())+ic.Gap()+text.Sanitize(projects[i].Name))
	}
	for i, ws := range workspaces {
		if i > 0 {
			proj = append(proj, "")
		}
		proj = append(proj, th.Muted.Render(ws))
		proj = append(proj, byWS[ws]...)
	}
	proj = append(proj, missing...)
	projShort := strconv.Itoa(len(st.Projects))
	if len(proj) == 0 {
		proj, projShort = []string{th.Warn.Render("none · p picks the projects to read")}, "none"
	}
	envs, envShort := []string{th.Muted.Render("every environment of the picked projects")}, "all"
	if len(st.Envs) > 0 {
		envs, envShort = envChips(ctx, st.Envs, w), strconv.Itoa(len(st.Envs))
	}
	write, writeShort := []string{th.Muted.Render("none · nothing can be written")}, "none"
	if len(st.WriteEnvs) > 0 {
		write, writeShort = envChips(ctx, st.WriteEnvs, w), strconv.Itoa(len(st.WriteEnvs))
		write = append(write, "", th.Muted.Render(tui.Plural(len(st.WriteEnvs), "environment")+" in "+tui.Plural(projectsOf(ctx, st.WriteEnvs), "project")))
	}
	leak, leakShort := []string{th.Muted.Render("not set")}, "—"
	if v := sinceText(st.Since); v != "" {
		leak, leakShort = []string{th.Accent.Render(ic.LeakTime() + ic.Gap() + v)}, v
		if _, err := time.Parse(time.RFC3339, st.Since); err == nil {
			leakShort = v[5:] // the year goes: "01-02 15:04"
		}
	}
	internal, internalShort := []string{th.Muted.Render("none · only the built-in names")}, "—"
	if len(st.Internal) > 0 {
		var chips []string
		for _, n := range st.Internal {
			chips = append(chips, th.Accent2.Render(ic.Names())+ic.Gap()+text.Sanitize(n))
		}
		internal, internalShort = wrapChips(chips, w, 0), strconv.Itoa(len(st.Internal))
	}
	mode := st.Icons
	if mode == "" {
		mode = "auto"
	}
	var modes []string
	for _, m := range []string{"auto", "nerd", "plain"} {
		mark, style := ic.NotChosen(), th.Muted
		if m == mode {
			mark, style = ic.Chosen(), th.Accent
		}
		modes = append(modes, style.Render(mark+ic.Gap()+m))
	}
	found := "no Nerd Font found"
	if ctx.NerdDetected {
		found = "a Nerd Font is installed"
	}
	return []setting{
		{key: "p", icon: ic.Projects(), label: "Projects", short: projShort,
			about: "The projects the app reads. Nothing else is listed or loaded.",
			value: proj, hint: "pick the projects"},
		{key: "e", icon: ic.Envs(), label: "Environments", short: envShort,
			about: "The environments shown in every screen; none picked shows them all.",
			value: envs, hint: "pick the environments"},
		{key: "w", icon: ic.Writable(), label: "Write envs", short: writeShort,
			about: "Where the app may change variables, limits and domains. Every other environment is read only.",
			value: write, hint: "pick the environments to write"},
		{key: "l", icon: ic.LeakTime(), label: "Leak time", short: leakShort,
			about: "When the secrets leaked. Values changed after it count as rotated; copying a value needs it.",
			value: leak, hint: "type it as YYYY-MM-DD HH:MM · empty clears it"},
		{key: "i", icon: ic.Names(), label: "Internal names", short: internalShort,
			about: "Variables you issue yourselves: Rotate generates them instead of asking for a value.",
			value: internal, hint: "type the names, comma separated"},
		{key: "g", icon: ic.Glyphs(), label: "Icons", short: mode,
			about: "Nerd Font glyphs, or plain ones every terminal shows. Auto picks by what is installed: " + found + ".",
			value: []string{strings.Join(modes, "   ")}, hint: "switch auto → nerd → plain"},
		s.account(ctx),
	}
}

// account is the Account entry: who the app acts as, its token and when
// Railway was last read. Nothing in it is changed here.
func (s *screen) account(ctx *tui.Ctx) setting {
	th, ic := ctx.Theme, ctx.Icons
	acct, short := text.Sanitize(ctx.Account), ic.Ok()
	if acct == "" {
		acct, short = th.Warn.Render("not signed in"), "—"
	}
	tok := text.Sanitize(ctx.Token)
	if tok == "" {
		tok = "unknown"
	}
	return setting{key: "a", icon: ic.Account(), label: "Account", short: short,
		about: "Who the app acts as: your own railway login. Every user has their own.",
		value: []string{
			th.Accent2.Render(ic.Account()) + ic.Gap() + acct,
			th.Accent2.Render(ic.Token()) + ic.Gap() + th.Muted.Render("token  ") + tok,
			th.Accent2.Render(ic.LastRead()) + ic.Gap() + th.Muted.Render("read   ") + s.lastRead(ctx),
		},
		hint: "railway login in a terminal signs in again"}
}

// projectsOf counts the projects of environment ids.
func projectsOf(ctx *tui.Ctx, ids []string) int {
	seen := map[string]bool{}
	for _, id := range ids {
		seen[ctx.Store.PlaceOf(id, "").ProjectID] = true
	}
	return len(seen)
}

// envTone colours an environment by what its name says: production red,
// staging and tests yellow, development green, any other blue.
func envTone(th tui.Theme, name string) lipgloss.Style {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "prod"):
		return th.Danger
	case strings.Contains(n, "stag"), strings.Contains(n, "qa"), strings.Contains(n, "test"):
		return th.Warn
	case strings.Contains(n, "dev"), strings.Contains(n, "preview"), strings.Contains(n, "local"):
		return th.OK
	}
	return th.Accent2
}

// envChips lists environments by project. An environment several projects
// have is in the same column in each of them, blank where a project has
// none, the most common first; a project's own environments follow and
// wrap under the first column.
func envChips(ctx *tui.Ctx, ids []string, w int) []string {
	th, ic := ctx.Theme, ctx.Icons
	var projectOrder []string
	envsOf := map[string][]string{} // project → environment names, in order
	for _, id := range ids {
		project, env, ok := strings.Cut(ctx.Store.EnvLabel(id), "/")
		if !ok {
			project, env = "?", id
		}
		project, env = text.Sanitize(project), text.Sanitize(env)
		if _, seen := envsOf[project]; !seen {
			projectOrder = append(projectOrder, project)
		}
		envsOf[project] = append(envsOf[project], env)
	}
	count := map[string]int{}
	for _, envs := range envsOf {
		for _, e := range envs {
			count[e]++
		}
	}
	var shared []string
	for e, n := range count {
		if n > 1 {
			shared = append(shared, e)
		}
	}
	slices.SortFunc(shared, func(a, b string) int { return cmp.Or(count[b]-count[a], strings.Compare(a, b)) })

	chip := func(env string) string { return envTone(th, env).Render(ic.EnvDot()) + ic.Gap() + env }
	projW := 0
	for _, p := range projectOrder {
		projW = max(projW, ansi.StringWidth(p))
	}
	projW = min(projW, max(4, w/3))
	room := max(8, w-projW-2)
	cellW := make([]int, len(shared))
	for i, e := range shared {
		cellW[i] = ansi.StringWidth(chip(e))
	}

	var out []string
	for _, p := range projectOrder {
		// the shared columns, then the project's own environments
		var cells []string
		last := -1
		for i, e := range shared {
			cell := strings.Repeat(" ", cellW[i])
			if slices.Contains(envsOf[p], e) {
				cell, last = tui.Pad(chip(e), cellW[i]), i
			}
			cells = append(cells, cell)
		}
		var own []string
		for _, e := range envsOf[p] {
			if !slices.Contains(shared, e) {
				own = append(own, chip(e))
			}
		}
		lead := strings.Join(cells[:last+1], "  ")
		lines := wrapChips(own, room, ansi.StringWidth(lead))
		if lead != "" {
			if len(lines) == 0 {
				lines = []string{lead}
			} else {
				lines[0] = lead + "  " + lines[0]
			}
		}
		for i, l := range lines {
			name := ""
			if i == 0 {
				name = p
			}
			out = append(out, th.Muted.Render(tui.Pad(name, projW))+"  "+ansi.Truncate(l, room, "…"))
		}
	}
	return out
}

// wrapChips lays chips out in lines of at most w cells, two spaces apart;
// the first line already holds first cells before them.
func wrapChips(chips []string, w, first int) []string {
	var out []string
	cur, curW := "", 0
	if first > 0 {
		curW = first // a placeholder: the caller puts what holds them
	}
	for _, c := range chips {
		cw := ansi.StringWidth(c)
		switch {
		case cur == "" && curW == 0:
			cur, curW = c, cw
		case cur == "" && curW+2+cw <= w:
			cur, curW = c, curW+2+cw
		case curW+2+cw <= w:
			cur, curW = cur+"  "+c, curW+2+cw
		default:
			out = append(out, cur)
			cur, curW = c, cw
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// wrapWords wraps prose to w cells.
func wrapWords(s string, w int) []string {
	return strings.Split(ansi.Wordwrap(s, w, " "), "\n")
}

func inventoryKeys(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = store.InventoryKey(id)
	}
	return out
}
