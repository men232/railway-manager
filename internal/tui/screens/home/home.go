// Package home is the bottom screen: counts of what is loaded, load
// progress and failures, runs in progress and the menu.
package home

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
)

type screen struct {
	stalled components.Stalled // the stalled-password banner
	// cursor is the key of the selected menu item, bannerKey for the
	// stalled banner; "" before the first View picks one.
	cursor string
	// banner is whether the last View showed the banner: the cursor goes
	// to it when it appears, as enter on it continues the stalled run.
	banner bool
}

// bannerKey is the cursor on the stalled banner.
const bannerKey = "!"

// New returns the Home screen.
func New() tui.Screen { return &screen{} }

func (*screen) Init(*tui.Ctx) tea.Cmd { return nil }

func (*screen) Title() string { return "Home" }

// Note is what is being read, at the right of the frame's header, so the
// body does not move while it loads; empty once all is read.
func (*screen) Note(ctx *tui.Ctx) string {
	if ctx.Store == nil {
		return ""
	}
	if ctx.Store.Load(store.CatalogKey).Loading {
		return ctx.Icons.Run() + " reading projects…"
	}
	targets := loaders.Targets(ctx.Store.Projects(), ctx.Settings)
	loading := 0
	for _, t := range targets {
		if ctx.Store.Load(store.InventoryKey(t.ProjectID)).Loading {
			loading++
		}
	}
	if loading == 0 {
		return ""
	}
	return fmt.Sprintf("%s reading variables %d/%d projects", ctx.Icons.Run(), len(targets)-loading, len(targets))
}

// Notice has no standing footer message.
func (*screen) Notice(*tui.Ctx) tui.FlashMsg { return tui.FlashMsg{} }

// Capturing reports whether the banner's question takes every key.
func (s *screen) Capturing() bool { return s.stalled.Capturing() }

type item struct {
	key, label, open, about string
	needsRead               bool
	icon                    func(tui.Icons) string
}

// menu is Home's items; a needsRead item is dimmed until a selected
// project's variables were read once (its key still opens it). Refresh
// and quit are keys only.
var menu = []item{
	{"s", "Services", "services", "status, charts and logs", true, tui.Icons.Services},
	{"v", "Variables", "variables", "by name or by value", true, tui.Icons.Variables},
	{"R", "Rotate secrets", "rotate", "find leaked secrets", true, tui.Icons.Rotate},
	{"c", "Change set", "changeset", "nothing staged", false, tui.Icons.ChangeSet},
	{"h", "History", "history", "who changed what", true, tui.Icons.History},
	{"o", "Settings", "settings", "what to read and write", false, tui.Icons.Settings},
}

// keys are the cursor's stops: the banner when shown, then the menu.
func (s *screen) keys(ctx *tui.Ctx) []string {
	var out []string
	if len(tui.StalledRuns(ctx)) > 0 {
		out = append(out, bannerKey)
	}
	for _, m := range menu {
		out = append(out, m.key)
	}
	return out
}

// sync puts the cursor on the banner when it appears, and on the first item
// at first or when the banner it was on goes.
func (s *screen) sync(ctx *tui.Ctx) {
	shown := len(tui.StalledRuns(ctx)) > 0
	switch {
	case shown && !s.banner:
		s.cursor = bannerKey
	case s.cursor == "", s.cursor == bannerKey && !shown:
		s.cursor = menu[0].key
	}
	s.banner = shown
}

// move moves the cursor by d stops, staying within them.
func (s *screen) move(ctx *tui.Ctx, d int) {
	ks := s.keys(ctx)
	i := max(0, slices.Index(ks, s.cursor))
	s.cursor = ks[min(max(i+d, 0), len(ks)-1)]
}

// act acts on menu key k: opens its screen, refreshes, or quits.
func act(k string) tea.Cmd {
	switch k {
	case "r":
		return tui.Emit(tui.RefreshMsg{})
	case "q":
		return tui.Emit(tui.QuitMsg{})
	}
	for _, m := range menu {
		if m.key == k && m.open != "" {
			return tui.Emit(tui.OpenMsg{Name: m.open})
		}
	}
	return nil
}

func (s *screen) Keys() []tui.KeyHint {
	if k := s.stalled.Keys(); k != nil {
		return k
	}
	return []tui.KeyHint{
		{Key: "↑↓", Label: "move", Rank: 1},
		{Key: "enter", Label: "open", Rank: 1},
		{Key: "r", Label: "refresh", Rank: 2},
		{Key: "s", Label: "services", Rank: 3, Optional: true},
		{Key: "v", Label: "variables", Rank: 3, Optional: true},
		{Key: "R", Label: "rotate", Rank: 3, Optional: true},
		{Key: "c", Label: "change set", Rank: 3, Optional: true},
		{Key: "h", Label: "history", Rank: 3, Optional: true},
		{Key: "o", Label: "settings", Rank: 3, Optional: true},
		{Key: "q", Label: "quit", Always: true},
	}
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if handled, cmd := s.stalled.Update(ctx, msg); handled {
		return s, cmd
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	s.sync(ctx)
	switch k.Type {
	case tea.KeyUp:
		s.move(ctx, -1)
	case tea.KeyDown, tea.KeyTab:
		s.move(ctx, 1)
	case tea.KeyHome:
		s.move(ctx, -len(menu)-1)
	case tea.KeyEnd:
		s.move(ctx, len(menu)+1)
	case tea.KeyEnter:
		if s.cursor == bannerKey {
			s.stalled.Enter(ctx)
			return s, nil
		}
		return s, act(s.cursor)
	case tea.KeyRunes:
		switch k := string(k.Runes); k {
		case "j":
			s.move(ctx, 1)
		case "k":
			s.move(ctx, -1)
		case "q":
			return s, nil // the root quits on q
		case "r":
			return s, act("r")
		default:
			if slices.ContainsFunc(menu, func(m item) bool { return m.key == k }) {
				s.cursor = k
				return s, act(k)
			}
		}
	}
	return s, nil
}

// logoFont is the logo's letters, two lines of half blocks each.
var logoFont = map[rune][2]string{
	'r': {"█▀█", "█▀▄"},
	'a': {"▄▀█", "█▀█"},
	'i': {"█", "█"},
	'l': {"█  ", "█▄▄"},
	'w': {"█ █ █", "▀▄▀▄▀"},
	'y': {"█▄█", " █ "},
	'-': {"▄▄", "  "},
	'm': {"█▀▄▀█", "█ ▀ █"},
	'n': {"█▄ █", "█ ▀█"},
	'g': {"█▀▀", "█▄█"},
	'e': {"█▀▀", "██▄"},
}

// logo is the name in large letters, a column apart, coloured along its
// width.
func logo(th tui.Theme) []string {
	var rows [2][]rune
	for i, r := range "railway-manager" {
		for line, part := range logoFont[r] {
			if i > 0 {
				rows[line] = append(rows[line], ' ')
			}
			rows[line] = append(rows[line], []rune(part)...)
		}
	}
	out := make([]string, 2)
	for line, row := range rows {
		var b strings.Builder
		for x, c := range row {
			if c == ' ' {
				b.WriteRune(c)
				continue
			}
			b.WriteString(th.Logo(float64(x) / float64(len(row)-1)).Render(string(c)))
		}
		out[line] = b.String()
	}
	return out
}

// tagline is the line under the logo.
const tagline = "stage · review · apply · resume"

// columnW is the width of Home's centred column.
const columnW = 76

func (s *screen) View(ctx *tui.Ctx, full, height int) string {
	th, ic := ctx.Theme, ctx.Icons
	// everything sits in a column centred on the screen
	width := min(full, columnW)
	var lines []string
	add := func(l string) { lines = append(lines, l) }
	// varying counts the lines that come and go (a banner, failures,
	// runs): the column's top is placed by the others, so it stays put
	varying := 0
	addVarying := func(l string) { add(l); varying++ }
	s.sync(ctx)

	// the header: the logo, or the name where it does not fit, and the
	// account beside it
	acct := text.Sanitize(ctx.Account)
	if acct == "" {
		acct = th.Warn.Render("not signed in")
	}
	if tok := text.Sanitize(ctx.Token); tok != "" {
		acct += th.Muted.Render(ic.Sep() + tok)
	}
	big := logo(th)
	if ansi.StringWidth(big[0]) <= width-2 && height >= 16 {
		for _, l := range big {
			add(" " + l)
		}
	} else {
		add(" " + th.Logo(0).Render("railway") + th.Logo(0.5).Render("-") + th.Logo(1).Render("manager"))
	}
	// the tagline, and the account at the right of the same line
	tag := " " + th.Muted.Italic(true).Render(tagline)
	if gap := width - ansi.StringWidth(tag) - ansi.StringWidth(acct) - 1; gap >= 3 {
		tag += strings.Repeat(" ", gap) + acct
	} else {
		add(tag)
		tag = " " + acct
	}
	add(tag)
	add("")

	// a stalled run's banner, the first stop of the cursor
	if banner := s.stalled.Lines(ctx, width); len(banner) > 0 {
		if s.cursor == bannerKey {
			banner[0] = th.Selected.Render(tui.Pad(ansi.Truncate(" "+ansi.Strip(banner[0]), width, "…"), width))
		}
		for _, l := range banner {
			addVarying(l)
		}
		addVarying("")
	}

	// what is loaded, then load progress and failures, then runs
	cat := ctx.Store.Load(store.CatalogKey)
	targets := loaders.Targets(ctx.Store.Projects(), ctx.Settings)
	if ctx.Settings == nil || len(ctx.Settings.Projects) == 0 {
		add(" " + th.Warn.Render("no projects selected · o settings"))
	} else {
		envs, vars := 0, 0
		services := map[string]bool{}
		for _, t := range targets {
			envs += len(t.Environments)
			for _, e := range t.Environments {
				for _, id := range e.ServiceIDs {
					services[id] = true
				}
			}
			vars += ctx.Store.VariableCountIn(t.ProjectID, loaders.EnvIDs([]railway.DiscoveryTarget{t}))
		}
		var parts []string
		for _, c := range []struct {
			n    int
			word string
		}{{len(targets), "project"}, {envs, "environment"}, {len(services), "service"}, {vars, "variable"}} {
			n, word, _ := strings.Cut(tui.Plural(c.n, c.word), " ")
			parts = append(parts, th.Accent.Render(n)+" "+th.Muted.Render(word))
		}
		add(" " + strings.Join(parts, th.Faint.Render(ic.Sep())))
	}
	// reading is in the header (Note); failures stay here until fixed
	if cat.Err != "" && !cat.Loading {
		addVarying(" " + th.Danger.Render(ic.Fail()+ic.Gap()+text.Sanitize(cat.Err)) + th.Muted.Render(" · r retries"))
	}
	read := false
	for _, t := range targets {
		st := ctx.Store.Load(store.InventoryKey(t.ProjectID))
		read = read || !st.ReadAt.IsZero()
		if st.Err != "" {
			addVarying(" " + th.Danger.Render(ic.Fail()+ic.Gap()+text.Sanitize(t.ProjectName+": "+st.Err)) + th.Muted.Render(" · r retries"))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ctx.Running)) {
		p := ctx.Running[id]
		if p.State != apply.RunRunning {
			continue
		}
		head := ic.Run() + " run " + text.Sanitize(id) + " applying"
		count := fmt.Sprintf("%d/%d", p.Done, p.Total)
		barW := min(24, width-1-ansi.StringWidth(head)-ansi.StringWidth(count)-4)
		l := " " + th.Warn.Render(head)
		if barW >= 6 {
			l += "  " + bar(th, p.Done, p.Total, barW)
		}
		addVarying(l + "  " + th.Muted.Render(count))
	}
	add("")

	// the menu
	labelW, keyCol := 16, width-1
	for _, m := range menu {
		about := m.about
		if live := liveHint(ctx, m.key, targets); live != "" {
			about = live
		}
		head := m.icon(ic) + ic.Gap() + tui.Pad(m.label, labelW)
		gap := max(2, keyCol-4-ansi.StringWidth(head)-ansi.StringWidth(about)-1)
		tail := strings.Repeat(" ", gap) + m.key
		switch {
		case m.key == s.cursor:
			add(" " + th.Accent.Render("▌ ") + th.Accent.Bold(true).Render(head) + th.Accent.Render(text.Sanitize(about)) + th.Accent.Render(tail))
		case m.needsRead && !read:
			add(" " + th.Muted.Render("  "+head+text.Sanitize(about)+tail))
		default:
			add(" " + "  " + head + th.Muted.Render(text.Sanitize(about)) + th.Accent.Render(tail))
		}
	}

	left := strings.Repeat(" ", (full-width)/2)
	for i, l := range lines {
		lines[i] = left + ansi.Truncate(l, width, "…")
	}
	if top := min((height-(len(lines)-varying))/3, height-len(lines)); top > 0 {
		lines = append(make([]string, top), lines...)
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// bar is a progress bar w cells wide for done of total.
func bar(th tui.Theme, done, total, w int) string {
	n := 0
	if total > 0 {
		n = min(w, done*w/total)
	}
	return th.Accent.Render(strings.Repeat("━", n)) + th.Faint.Render(strings.Repeat("─", w-n))
}

// liveHint is what an item holds now, for those that say it: services
// down, the leak time, the change set, the settings; "" for the others.
func liveHint(ctx *tui.Ctx, key string, targets []railway.DiscoveryTarget) string {
	switch key {
	case "s":
		if ctx.Views == nil {
			return ""
		}
		rows := ctx.Views.Monitor(ctx.CS, loaders.EnvIDs(targets))
		if len(rows) == 0 {
			return ""
		}
		down := 0
		for _, r := range rows {
			if r.Status.Kind == monitor.Failed || r.Status.Kind == monitor.Offline {
				down++
			}
		}
		h := tui.Plural(len(rows), "service")
		if down > 0 {
			h += ctx.Icons.Sep() + fmt.Sprint(down) + " down"
		}
		return h
	case "R":
		if t := leakTime(ctx); t != "" {
			return "leak " + t
		}
	case "c":
		return strings.TrimPrefix(tui.Indicator(ctx), ctx.Icons.Pending()+" ")
	case "o":
		return settingsHint(ctx)
	}
	return ""
}

// leakTime is the leak time as the app shows it; "" when unset.
func leakTime(ctx *tui.Ctx) string {
	if ctx.Settings == nil || ctx.Settings.Since == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ctx.Settings.Since)
	if err != nil {
		return ""
	}
	return t.In(time.Local).Format("01-02 15:04")
}

// settingsHint is "N projects · M write environments".
func settingsHint(ctx *tui.Ctx) string {
	if ctx.Settings == nil {
		return ""
	}
	return tui.Plural(len(ctx.Settings.Projects), "project") + ctx.Icons.Sep() + tui.Plural(len(ctx.Settings.WriteEnvs), "write env")
}
