package rotate

import (
	"os"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
)

// addStep is where the add flow is.
type addStep int

const (
	addName   addStep = iota // the name input
	addPlaces                // the place multi-select
	addValue                 // g, p, f or r
	addMask                  // a masked paste
	addPick                  // the file picker
	addRead                  // a file read in flight
	addRef                   // a reference input
)

// adding is the add flow of the list: a name absent
// from the selected environments, places of the write environments, then
// a value for all of them, one variable.set per place in the group
// rotation.AddGroupKey(name).
type adding struct {
	step   addStep
	name   string
	text   *components.TextInput
	mask   *components.MaskedInput
	picker *components.FilePicker
	// cands are the places offered; chosen their keys.
	cands  []domain.Place
	chosen map[string]bool
	cursor int
}

func newAdding() *adding {
	return &adding{step: addName, text: &components.TextInput{Label: "name"}, chosen: map[string]bool{}}
}

func (a *adding) keys() []tui.KeyHint {
	switch a.step {
	case addPlaces:
		return []tui.KeyHint{
			{Key: "space", Label: "select", Always: true},
			{Key: "enter", Label: "confirm", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	case addValue:
		return []tui.KeyHint{
			{Key: "g", Label: "generate", Always: true},
			{Key: "p", Label: "paste", Always: true},
			{Key: "f", Label: "file", Always: true},
			{Key: "r", Label: "reference", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	case addPick:
		return []tui.KeyHint{{Key: "enter", Label: "pick", Always: true}, {Key: "esc", Label: "cancel", Always: true}}
	case addRead:
		return []tui.KeyHint{{Key: "esc", Label: "cancel", Always: true}}
	}
	return []tui.KeyHint{{Key: "enter", Label: "confirm", Always: true}, {Key: "esc", Label: "cancel", Always: true}}
}

// writePlaces is every place (services and shared scopes) of the selected
// environments that are write environments, in tree order, but for the
// places of databases.
func writePlaces(ctx *tui.Ctx) []domain.Place {
	if ctx.Settings == nil {
		return nil
	}
	envs := envIDs(ctx)
	an := rotation.Analyze(ctx.Store, rotation.Options{EnvIDs: envs, AllEnvIDs: allEnvIDs(ctx)})
	var out []domain.Place
	for _, p := range ctx.Views.TreePlaces(envs) {
		if slices.Contains(ctx.Settings.WriteEnvs, p.EnvID) && !an.IsDBPlace(p) {
			out = append(out, p)
		}
	}
	return out
}

// nameExists reports whether a place of the selected environments has the
// variable, in Railway or pending.
func nameExists(ctx *tui.Ctx, name string) bool {
	for _, p := range ctx.Views.TreePlaces(envIDs(ctx)) {
		for _, v := range ctx.Views.PlaceVariables(ctx.CS, p) {
			if v.Name == name {
				return true
			}
		}
	}
	return false
}

// addUpdate runs the add flow; it closes s.add when done or cancelled.
func (s *screen) addUpdate(ctx *tui.Ctx, msg tea.Msg) tea.Cmd {
	a := s.add
	switch m := msg.(type) {
	case components.CancelMsg:
		s.add = nil
	case components.SubmitMsg:
		switch a.step {
		case addName:
			return s.addNamed(ctx, strings.TrimSpace(m.Value))
		case addMask:
			a.mask = nil
			return s.addPut(ctx, m.Value)
		case addRef:
			a.text = nil
			if len(domain.ParseReferences(m.Value)) == 0 {
				a.step = addValue
				return tui.Flash('✗', "not a reference · p pastes a value")
			}
			return s.addPut(ctx, m.Value)
		}
	case components.FilePickedMsg:
		if a.step != addPick {
			return nil
		}
		a.picker, a.step = nil, addRead
		s.addSeq++
		return readFile(m.Path, s.addSeq)
	case fileReadMsg:
		if a.step != addRead || m.seq != s.addSeq {
			return nil
		}
		if m.err != "" {
			a.step = addValue
			return tui.Flash('✗', m.err)
		}
		return s.addPut(ctx, string(m.value))
	case tea.KeyMsg:
		return s.addKey(ctx, m)
	}
	return nil
}

func (s *screen) addKey(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	a := s.add
	switch a.step {
	case addName, addRef:
		return a.text.Update(k)
	case addMask:
		return a.mask.Update(k)
	case addPick:
		return a.picker.Update(k)
	}
	if k.Type == tea.KeyEsc || k.Type == tea.KeyCtrlC {
		s.add = nil
		return nil
	}
	if a.step == addRead {
		return nil
	}
	key := k.String()
	if a.step == addPlaces {
		switch {
		case k.Type == tea.KeyUp || key == "k":
			a.cursor = max(0, a.cursor-1)
		case k.Type == tea.KeyDown || key == "j":
			a.cursor = min(len(a.cands)-1, a.cursor+1)
		case k.Type == tea.KeyPgUp:
			a.cursor = max(0, a.cursor-page(ctx))
		case k.Type == tea.KeyPgDown:
			a.cursor = min(len(a.cands)-1, a.cursor+page(ctx))
		case key == " ":
			pk := a.cands[a.cursor].Key()
			if a.chosen[pk] {
				delete(a.chosen, pk)
			} else {
				a.chosen[pk] = true
			}
		case k.Type == tea.KeyEnter:
			if len(a.chosen) == 0 {
				return tui.Flash('!', "choose at least one place · space selects")
			}
			a.step = addValue
		}
		return nil
	}
	// addValue
	switch key {
	case "g":
		v, err := rotation.Generate(0, false)
		if err != nil {
			return tui.Flash('✗', "cannot generate: "+err.Error())
		}
		return s.addPut(ctx, v)
	case "p":
		a.step = addMask
		a.mask = &components.MaskedInput{Label: a.name + " · " + placesText(len(a.chosen)), Repeat: true}
	case "f":
		home, _ := os.UserHomeDir()
		a.picker = components.NewFilePicker(components.OSDirReader{}, home)
		a.picker.Theme = ctx.Theme
		a.step = addPick
	case "r":
		a.step = addRef
		a.text = &components.TextInput{Label: "reference"}
	}
	return nil
}

// addNamed checks the name and moves to the places.
func (s *screen) addNamed(ctx *tui.Ctx, name string) tea.Cmd {
	// An empty value validates the name alone.
	if _, err := kinds.SetVariable(ctx.Store, nil, domain.Place{}, name, "", ctx.Clock()()); err != nil {
		return tui.Flash('✗', err.Error())
	}
	if nameExists(ctx, name) {
		return tui.Flash('!', name+" exists · rotate it instead")
	}
	cands := writePlaces(ctx)
	if len(cands) == 0 {
		return tui.Flash('!', "no write environment · Settings w")
	}
	a := s.add
	a.step, a.name, a.text, a.cands, a.cursor = addPlaces, name, nil, cands, 0
	return nil
}

// addPut puts the value at the chosen places, resolved again: the name
// still absent and every place still offered.
func (s *screen) addPut(ctx *tui.Ctx, value string) tea.Cmd {
	a := s.add
	if value == "" {
		a.step = addValue
		return tui.Flash('✗', "the value is empty")
	}
	changed := func() tea.Cmd {
		s.add = nil
		return tui.Flash('!', "the places changed since a · a again")
	}
	if nameExists(ctx, a.name) {
		return changed()
	}
	cands := writePlaces(ctx)
	var places []domain.Place
	for _, p := range cands {
		if a.chosen[p.Key()] {
			places = append(places, p)
		}
	}
	if len(places) != len(a.chosen) {
		return changed()
	}
	group := rotation.AddGroupKey(a.name)
	now := ctx.Clock()()
	puts := make([]changeset.Change, 0, len(places))
	for _, p := range places {
		c, err := kinds.SetVariable(ctx.Store, pending(ctx), p, a.name, value, now)
		if err != nil {
			a.step = addValue
			return tui.Flash('✗', err.Error())
		}
		c.Group = group
		c.Origin = &changeset.Origin{Producer: producer, Row: group}
		puts = append(puts, c)
	}
	name := a.name
	s.add = nil
	return changes(nil, puts, '✓', placesText(len(places))+": "+name+" added")
}

// addView draws the add flow in the body.
func (s *screen) addView(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	a := s.add
	cut := func(l string) string { return ansi.Truncate(l, width, "…") }
	var out []string
	switch a.step {
	case addName:
		out = append(out, th.Accent.Render(cut("add a variable · its name")), a.text.View(width))
	case addPlaces:
		out = append(out, th.Accent.Render(cut("add "+text.Sanitize(a.name)+" · choose the places")))
		out = append(out, s.addPlacesView(ctx, width, height-1)...)
	default:
		out = append(out, th.Accent.Render(cut("add "+text.Sanitize(a.name)+" at "+placesText(len(a.chosen)))))
		out = append(out, cut("value: g generate (24 letters and digits) · p paste · f file · r reference"))
		switch a.step {
		case addMask:
			out = append(out, th.Warn.Render(a.mask.View(width)))
		case addRef:
			out = append(out, a.text.View(width))
		case addRead:
			out = append(out, th.Muted.Render("reading the file…"))
		case addPick:
			for len(out) < height-components.FilePickerHeight {
				out = append(out, "")
			}
			out = append(out, strings.Split(a.picker.View(width), "\n")...)
		}
	}
	if len(out) > height {
		out = out[:max(height, 0)]
	}
	return strings.Join(out, "\n")
}

// addPlacesView is the places by environment, the cursor's line in view.
func (s *screen) addPlacesView(ctx *tui.Ctx, width, height int) []string {
	th := ctx.Theme
	ad := s.add
	short := shortEnvs(ctx)
	var lines []string
	cur := 0
	for i, p := range ad.cands {
		if i == 0 || ad.cands[i-1].EnvID != p.EnvID {
			lines = append(lines, th.Faint.Render(ansi.Truncate("── "+envText(short, ctx.Store.EnvLabel(p.EnvID)), width, "…")))
		}
		mark, box := "  ", "[ ]"
		if i == ad.cursor {
			mark, cur = th.Accent.Render("▸")+" ", len(lines)
		}
		if ad.chosen[p.Key()] {
			box = "[x]"
		}
		lines = append(lines, ansi.Truncate(mark+box+" "+text.Sanitize(ctx.Store.ServiceName(p.ServiceID)), width, "…"))
	}
	if height < 1 {
		return nil
	}
	from := max(0, min(cur-height+1, len(lines)-height))
	return lines[from:min(len(lines), from+height)]
}
