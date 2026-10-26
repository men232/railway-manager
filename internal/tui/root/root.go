// Package root is the root Bubble Tea model: it owns the screen stack, applies
// loader data to the store, acts on the screens' intents and drives runs.
// Screen logic lives in the screens.
package root

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/rotation"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/auth"
	"github.com/men232/railway-manager/internal/tui/screens/home"
	"github.com/men232/railway-manager/internal/tui/screens/keys"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
	"github.com/men232/railway-manager/internal/view"
	"github.com/men232/railway-manager/internal/workdir"
)

// Deps is what the root model is built from.
//
// Run goroutines never touch the live Store: the root publishes a deep copy
// to Snapshot at startup and after every store.Update, and the Runs' NewEnv
// (Env.Store) and Compile must read Snapshot.Load(). Likewise
// Client.WriteAllowed must read WriteEnvs, never the Settings the screens
// edit; the root publishes it at startup and on every SaveSettingsMsg. The
// program wiring (Task 19) sets them up that way.
type Deps struct {
	Client                      *railway.Client
	Store                       *store.Store
	Views                       *view.Views
	Reg                         apply.Registry
	Runs                        *apply.Runs
	Settings                    *settings.Settings
	SettingsPath, ChangeSetPath string
	// Snapshot holds the store copy run goroutines read.
	Snapshot *atomic.Pointer[store.Store]
	// WriteEnvs holds the writable environment ids WriteAllowed reads.
	WriteEnvs *atomic.Pointer[map[string]bool]
	// CS is the live change set. The root mutates it in place and never
	// replaces the pointer.
	CS *changeset.ChangeSet
	// LoadNote is shown at startup with !, e.g. a corrupt change set moved
	// aside.
	LoadNote string
	Now      func() time.Time
	// Send delivers a message to the program from another goroutine
	// (tea.Program.Send); run events travel through it.
	Send func(tea.Msg)
	// Live runs Railway subscriptions (Ctx.Live); nil: screens poll only.
	Live *railway.Live
	// Screens builds screens by name for OpenMsg and Ctx.Open. "keys" is
	// provided when missing.
	Screens map[string]func(args ...any) tui.Screen
	Theme   tui.Theme
	Icons   tui.Icons
	IsTTY   bool
	// NerdDetected is the startup font scan result (Ctx.NerdDetected).
	NerdDetected bool
	// TokenInfo describes the token source and expiry for Ctx.Token, never
	// the token itself. It is read at startup and after each successful
	// WhoAmI (a sign-in may change the source).
	TokenInfo func() string
	// SelectProjects and SelectEnvs (names or ids, from --project and
	// --env) override the saved selection for this run. They are resolved
	// once, after the first catalog read; unknown ones are flashed. Saving
	// settings keeps the saved selection unless it was edited meanwhile.
	SelectProjects, SelectEnvs []string
}

const quitPrompt = "a run is applying: quit anyway? it can be resumed"

// quitWait bounds how long quitting waits for cancelled runs to end.
var quitWait = 3 * time.Second

// freezeQuitWait bounds how much longer quitting waits, after quitWait, for
// a run being frozen (see quitTimeout).
var freezeQuitWait = 30 * time.Second

// quitTimeoutMsg ends the wait for cancelled runs; freezeTimeoutMsg ends
// the longer wait for a run being frozen.
type (
	quitTimeoutMsg   struct{}
	freezeTimeoutMsg struct{}
)

// Model is the root model. Home is always at the bottom of the stack.
type Model struct {
	d     Deps
	ctx   *tui.Ctx
	stack []tui.Screen
	flash tui.FlashMsg

	quitAsk *components.Confirm

	saver   *saver
	saveSeq uint64

	// staleHeld are the run ids holding live changes at startup; Init's
	// startup unlock considers these only (unlockStale).
	staleHeld []string

	askedProjects bool // the startup project picker was offered

	quitting bool // runs were cancelled; quit when they end or quitWait passes

	cancels     map[string]context.CancelFunc
	steps       map[string]map[string]string // run → step key → status
	firstErr    map[string]string            // run → first step error reported
	runProjects map[string][]string

	// The runs index read (loaders.Runs): at most one in flight and one
	// queued behind it.
	runsInFlight, runsQueued bool
	runsWarned               string // the skipped-run warnings last flashed

	preparing bool   // a prepare is pending: from the apply until its prepare lock is released
	freezing  string // the run a prepare freezes; its changes are locked in the live set

	abandoning map[string]bool // runs whose Abandon is on disk
	// revertID and revertLabel are a revert put after its abandon whose run
	// is being prepared: if it does not start, the flash says it is pending.
	revertID, revertLabel string

	invSeq     uint64            // inventory request sequence
	invApplied map[string]uint64 // project → seq of the last applied inventory

	sel selection // the --project/--env override
}

// selection is the --project/--env override of this run: what it set and
// the saved values it replaced.
type selection struct {
	resolved                 bool
	projects, envs           []string // set by the override; nil when not overridden
	savedProjects, savedEnvs []string
}

// saver serialises change-set saves so an older snapshot never lands after
// a newer one.
type saver struct {
	mu      sync.Mutex
	last    uint64
	lastErr error // the result of writing snapshot last
}

// New builds the root model with Home on the stack. It notes the runs that
// hold live changes; Init unlocks those that are finished, abandoned or gone
// (a crash between the end of a run and its Reconcile).
func New(d Deps) *Model {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Send == nil {
		d.Send = func(tea.Msg) {}
	}
	if d.CS == nil {
		d.CS = &changeset.ChangeSet{}
	}
	if d.Settings == nil {
		d.Settings = &settings.Settings{}
	}
	if d.Theme.Renderer == nil {
		d.Theme = tui.NewTheme(lipgloss.ColorProfile())
	}
	screens := maps.Clone(d.Screens)
	if screens == nil {
		screens = map[string]func(args ...any) tui.Screen{}
	}
	if screens["keys"] == nil {
		screens["keys"] = keys.New
	}
	d.Screens = screens

	m := &Model{
		d:           d,
		saver:       &saver{},
		cancels:     map[string]context.CancelFunc{},
		steps:       map[string]map[string]string{},
		firstErr:    map[string]string{},
		runProjects: map[string][]string{},
		invApplied:  map[string]uint64{},
		abandoning:  map[string]bool{},
	}
	m.ctx = &tui.Ctx{
		API: d.Client, Store: d.Store, Views: d.Views, CS: d.CS, Reg: d.Reg, Runs: d.Runs,
		Settings: d.Settings, Theme: d.Theme, Icons: d.Icons, IsTTY: d.IsTTY, Now: d.Now,
		NerdDetected: d.NerdDetected, Live: d.Live, Send: d.Send,
		Running: map[string]tui.RunProgress{},
		Filters: map[string]string{},
		Open: func(name string, args ...any) tui.Screen {
			if f := screens[name]; f != nil {
				return f(args...)
			}
			return nil
		},
	}
	m.refreshToken()
	m.stack = []tui.Screen{home.New()}
	if d.LoadNote != "" {
		m.flash = tui.FlashMsg{Kind: '!', Text: d.LoadNote}
	}
	for _, c := range d.CS.Changes {
		if c.RunID != "" && !slices.Contains(m.staleHeld, c.RunID) {
			m.staleHeld = append(m.staleHeld, c.RunID)
		}
	}
	slices.Sort(m.staleHeld)
	m.publishSnapshot()
	m.publishWriteEnvs()
	return m
}

func (m *Model) publishSnapshot() {
	if m.d.Snapshot != nil {
		m.d.Snapshot.Store(m.d.Store.Clone())
	}
}

func (m *Model) publishWriteEnvs() {
	if m.d.WriteEnvs == nil {
		return
	}
	w := make(map[string]bool, len(m.d.Settings.WriteEnvs))
	for _, id := range m.d.Settings.WriteEnvs {
		w[id] = true
	}
	m.d.WriteEnvs.Store(&w)
}

func (m *Model) refreshToken() {
	if m.d.TokenInfo != nil {
		m.ctx.Token = m.d.TokenInfo()
	}
}

// Ctx is the shared screen context.
func (m *Model) Ctx() *tui.Ctx { return m.ctx }

// staleRunsMsg is the runs list the startup unlock read for the run ids
// held at startup.
type staleRunsMsg struct {
	held  []string
	infos []apply.RunInfo
	errs  []error
}

// unlockStale reads the runs in a command for the startup unlock of the
// runs noted in New; staleRead unlocks. Until then their changes stay held.
func (m *Model) unlockStale() tea.Cmd {
	if m.d.Runs == nil || len(m.staleHeld) == 0 {
		return nil
	}
	held, runs := m.staleHeld, m.d.Runs
	m.staleHeld = nil
	return func() tea.Msg {
		infos, errs := runs.List()
		return staleRunsMsg{held: held, infos: infos, errs: errs}
	}
}

// staleRead releases the live changes of the runs held at startup that are
// no longer unfinished, and saves. A run started since is never one of them.
func (m *Model) staleRead(msg staleRunsMsg) tea.Cmd {
	if slices.ContainsFunc(msg.errs, func(err error) bool { return errors.Is(err, apply.ErrRunsUnreadable) }) {
		// No run is known, so none is known gone.
		m.fail("runs directory is unreadable: changes held by runs stay held")
		return nil
	}
	state := map[string]string{}
	for _, in := range msg.infos {
		state[in.ID] = in.State
	}
	unlocked := false
	for _, id := range msg.held {
		if slices.ContainsFunc(msg.errs, func(err error) bool {
			var re *apply.RunError
			return errors.As(err, &re) && re.RunID == id
		}) {
			continue // a corrupt run is not known gone: its changes stay held
		}
		if _, running := m.cancels[id]; running || m.abandoning[id] || id == m.freezing {
			// Defensive only: a held id is captured in New, and a run this
			// process starts draws an id no live change carries.
			continue
		}
		if s, ok := state[id]; !ok || s == apply.RunFinished || s == apply.RunAbandoned {
			if slices.ContainsFunc(m.d.CS.Changes, func(c changeset.Change) bool { return c.RunID == id }) {
				m.d.CS.Unlock(id, nil)
				unlocked = true
			}
		}
	}
	if !unlocked {
		return nil
	}
	return m.saveCS()
}

// startupPickedMsg is the startup project picker's answer.
type startupPickedMsg struct{ ids []string }

// askProjects offers the project picker once, when nothing is selected after
// the catalog loaded. Cancelling leaves Home's "no projects selected" line.
func (m *Model) askProjects() tea.Cmd {
	if m.askedProjects || len(m.d.Settings.Projects) > 0 || m.ctx.Open == nil {
		return nil
	}
	projects := m.d.Store.Projects()
	if len(projects) == 0 {
		return nil
	}
	items := make([]picker.Item, len(projects))
	for i, p := range projects {
		items[i] = picker.Item{ID: p.ID, Label: p.Name, Note: p.Workspace}
	}
	done := func(ids []string) tea.Msg { return startupPickedMsg{ids: ids} }
	s := m.ctx.Open("picker", "Select projects", items, map[string]bool{}, done)
	if s == nil {
		return nil
	}
	m.askedProjects = true
	return m.push(s)
}

// Init starts the reads: WhoAmI, then (on its answer) the catalog, then the
// inventory of the selected projects; the runs index; and the startup
// unlock of stale held changes.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.top().Init(m.ctx), loaders.WhoAmI(m.d.Client), m.refreshRuns(), m.unlockStale())
}

func (m *Model) top() tui.Screen { return m.stack[len(m.stack)-1] }

// Update applies data to the store, acts on intents and global keys, and
// forwards everything else to the top screen.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// the top screen sees the new size in its context
		m.ctx.Width, m.ctx.Height = msg.Width, msg.Height
		return m, m.forward(msg)
	case tea.KeyMsg:
		return m, m.key(msg)
	case quitTimeoutMsg:
		return m, m.quitTimeout()
	case freezeTimeoutMsg:
		return m, tea.Quit
	case tui.PushMsg:
		return m, m.push(msg.Screen)
	case tui.PopMsg:
		return m, m.pop()
	case tui.OpenMsg:
		return m, m.open(msg.Name, msg.Args...)
	case tui.FlashMsg:
		m.flash = msg
		return m, nil
	case tui.PutChangeMsg:
		return m, m.put(msg.Change)
	case tui.RemoveChangeMsg:
		return m, m.remove(msg.ID)
	case tui.ChangesMsg:
		return m, m.changes(msg)
	case tui.QuitMsg:
		return m, m.quit()
	case tui.ToggleRedeployMsg:
		m.d.CS.ToggleRedeploy(msg.PlaceKey)
		return m, m.saveCS()
	case tui.ApplyMsg:
		return m, m.apply(msg.IDs)
	case tui.ImmediateMsg:
		return m, m.immediate(msg.Change)
	case preparedMsg:
		return m, m.prepared(msg)
	case checkedMsg:
		return m, m.checked(msg)
	case frozenMsg:
		return m, m.frozen(msg)
	case immediateMsg:
		return m, m.immediatePrepared(msg)
	case releasedMsg:
		return m, m.released(msg)
	case tui.ResumeMsg:
		return m, m.resume(msg.RunID)
	case resumeReadMsg:
		return m, m.resumeRead(msg)
	case tui.AbandonMsg:
		return m, m.abandon(msg.RunID)
	case abandonedMsg:
		return m, m.abandoned(msg)
	case tui.RevertMsg:
		return m, m.revert(msg)
	case revertReadMsg:
		return m, m.revertRead(msg)
	case revertedMsg:
		return m, m.reverted(msg)
	case staleRunsMsg:
		return m, m.staleRead(msg)
	case tui.RefreshRunsMsg:
		return m, m.refreshRuns()
	case loaders.RunsMsg:
		return m, tea.Batch(m.runsRead(msg), m.forward(msg))
	case tui.SaveSettingsMsg:
		m.publishWriteEnvs()
		return m, m.saveSettings()
	case tui.RefreshMsg:
		return m, m.startCatalog()
	case tui.RunEventMsg:
		m.event(msg.Event)
		return m, tea.Batch(m.forward(msg), m.refreshRuns())
	case tui.RunDoneMsg:
		cmd := m.runDone(msg)
		cmd = tea.Batch(cmd, m.forward(msg), m.refreshRuns())
		if q := m.quitIfIdle(); q != nil {
			// The change-set save queued after Reconcile must land first.
			return m, tea.Sequence(cmd, q)
		}
		return m, cmd
	case loaders.AccountMsg:
		return m, m.account(msg)
	case startupPickedMsg:
		if len(msg.ids) == 0 {
			return m, nil
		}
		m.d.Settings.Projects = msg.ids
		return m, tea.Batch(m.saveSettings(), m.startCatalog())
	case loaders.InventoryMsg:
		if msg.Seq < m.invApplied[msg.ProjectID] {
			return m, nil // a late answer to an older request
		}
		m.invApplied[msg.ProjectID] = msg.Seq
	}

	if u, ok := msg.(store.Update); ok {
		u.ApplyTo(m.d.Store)
		m.publishSnapshot()
	}
	cmds := []tea.Cmd{m.forward(msg)}
	if ae := authError(msg); ae != nil {
		cmds = append(cmds, m.signIn(ae.Msg))
	}
	switch msg := msg.(type) {
	case loaders.CatalogMsg:
		if msg.Err == nil {
			m.resolveSelection()
			cmds = append(cmds, m.startInventory(m.d.Settings.Projects))
			cmds = append(cmds, m.askProjects())
		}
	}
	return m, tea.Batch(cmds...)
}

// resolveSelection applies --project and --env once, against the catalog
// just read. A project matches by id or name; an env matches by id or name
// within the selected projects (all projects when none is selected). When
// nothing given matches, the saved selection stays.
func (m *Model) resolveSelection() {
	if m.sel.resolved || len(m.d.SelectProjects)+len(m.d.SelectEnvs) == 0 {
		return
	}
	m.sel.resolved = true
	projects := m.d.Store.Projects()
	var notes []string
	if len(m.d.SelectProjects) > 0 {
		var ids []string
		for _, a := range m.d.SelectProjects {
			found := false
			for _, p := range projects {
				if p.ID == a || p.Name == a {
					found = true
					if !slices.Contains(ids, p.ID) {
						ids = append(ids, p.ID)
					}
				}
			}
			if !found {
				notes = append(notes, "no project "+a)
			}
		}
		if len(ids) > 0 {
			m.sel.savedProjects = slices.Clone(m.d.Settings.Projects)
			m.sel.projects = ids
			m.d.Settings.Projects = slices.Clone(ids)
		}
	}
	if len(m.d.SelectEnvs) > 0 {
		var ids []string
		for _, a := range m.d.SelectEnvs {
			found := false
			for _, p := range projects {
				if len(m.d.Settings.Projects) > 0 && !slices.Contains(m.d.Settings.Projects, p.ID) {
					continue
				}
				for _, e := range p.Environments {
					if e.ID == a || e.Name == a {
						found = true
						if !slices.Contains(ids, e.ID) {
							ids = append(ids, e.ID)
						}
					}
				}
			}
			if !found {
				notes = append(notes, "no env "+a)
			}
		}
		if len(ids) > 0 {
			m.sel.savedEnvs = slices.Clone(m.d.Settings.Envs)
			m.sel.envs = ids
			m.d.Settings.Envs = slices.Clone(ids)
		}
	}
	if len(notes) > 0 {
		m.flash = tui.FlashMsg{Kind: '!', Text: strings.Join(notes, " · ")}
	}
}

// authError finds a refused token in a loader result.
func authError(msg tea.Msg) *railway.AuthError {
	var err error
	switch msg := msg.(type) {
	case loaders.AccountMsg:
		err = msg.Err
	case loaders.CatalogMsg:
		err = msg.Err
	case loaders.InventoryMsg:
		err = msg.Err
	}
	var ae *railway.AuthError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// account handles WhoAmI's answer wherever Auth is on the stack: success
// removes every Auth and reads the catalog again; failure shows the reason
// on the Auth screen (pushing one for a refused token).
func (m *Model) account(msg loaders.AccountMsg) tea.Cmd {
	if msg.Err == nil {
		if m.ctx.Views != nil {
			m.ctx.Views.Me = msg.Account.ID
		}
		m.ctx.Account = msg.Account.Email
		if m.ctx.Account == "" {
			m.ctx.Account = msg.Account.Name
		}
		m.refreshToken()
		return tea.Batch(m.removeAuth(), m.startCatalog())
	}
	if ae := authError(msg); ae != nil {
		return m.signIn(ae.Msg)
	}
	if a := m.findAuth(); a != nil {
		a.SetReason(msg.Err.Error())
		return nil
	}
	m.flash = tui.FlashMsg{Kind: '✗', Text: msg.Err.Error()}
	return m.startCatalog()
}

func (m *Model) findAuth() *auth.Screen {
	for _, s := range m.stack {
		if a, ok := s.(*auth.Screen); ok {
			return a
		}
	}
	return nil
}

// removeAuth drops every Auth screen from the stack; when the top changes,
// the new top is resumed.
func (m *Model) removeAuth() tea.Cmd {
	old := m.top()
	m.stack = slices.DeleteFunc(m.stack, func(s tui.Screen) bool {
		_, ok := s.(*auth.Screen)
		return ok
	})
	if m.top() == old {
		return nil
	}
	if r, ok := m.top().(tui.Resumer); ok {
		return r.Resume(m.ctx)
	}
	return nil
}

// signIn shows the reason on the Auth screen anywhere on the stack, or
// pushes one.
func (m *Model) signIn(reason string) tea.Cmd {
	if a := m.findAuth(); a != nil {
		a.SetReason(reason)
		return nil
	}
	return m.push(auth.New(reason))
}

func (m *Model) forward(msg tea.Msg) tea.Cmd {
	next, cmd := m.top().Update(m.ctx, msg)
	if next != nil {
		m.stack[len(m.stack)-1] = next
	}
	return cmd
}

func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	if m.quitting {
		if k.Type == tea.KeyCtrlC {
			return tea.Quit
		}
		return nil
	}
	if m.quitAsk != nil {
		cmd := m.quitAsk.Update(k)
		m.quitAsk = nil
		if cmd != nil {
			if _, ok := cmd().(components.ConfirmedMsg); ok {
				return m.quitNow()
			}
		}
		return nil
	}
	m.flash = tui.FlashMsg{}
	top := m.top()
	if c, ok := top.(tui.Capturer); ok && c.Capturing() {
		return m.forward(k)
	}
	if c, ok := top.(tui.KeyClaimer); ok && k.Type != tea.KeyCtrlC && c.Claims(k.String()) {
		return m.forward(k)
	}
	switch k.String() {
	case "ctrl+c", "q", "esc":
		if len(m.stack) == 1 {
			return m.quit()
		}
		return m.pop()
	case "C":
		return m.open("changeset")
	case "?":
		if top.Title() == "Keys" {
			return nil
		}
		return m.open("keys", top.Title(), top.Keys())
	}
	return m.forward(k)
}

// quit asks first while a run of this process is applying.
func (m *Model) quit() tea.Cmd {
	for _, p := range m.ctx.Running {
		if p.State == apply.RunRunning {
			m.quitAsk = &components.Confirm{Prompt: quitPrompt}
			return nil
		}
	}
	return m.quitNow()
}

// quitNow stops the runs of this process; they stay unfinished on disk and
// can be resumed. It quits once they have all reported RunDone and a
// pending prepare has released its lock, or after quitWait.
func (m *Model) quitNow() tea.Cmd {
	if len(m.cancels) == 0 && !m.preparing {
		return tea.Quit
	}
	for _, cancel := range m.cancels {
		cancel()
	}
	m.quitting = true
	return tea.Tick(quitWait, func(time.Time) tea.Msg { return quitTimeoutMsg{} })
}

// quitTimeout quits when quitWait passed, unless a run is being frozen: its
// changes are held in the live set saved just before the freeze, and the
// freeze's result must land so the run is executed or its changes released.
// That wait is capped by freezeQuitWait, so the wait for cancelled runs can
// grow from quitWait to quitWait+freezeQuitWait (30 s more) when a freeze
// was in flight. Quitting at the cap is an accepted
// risk: the freeze command dies with the process. The live set on disk
// already holds the run's changes, so a run written whole is resumable
// after a restart, and a run left without its journal is not a run: the
// startup unlock releases its changes.
func (m *Model) quitTimeout() tea.Cmd {
	if m.freezing == "" {
		return tea.Quit
	}
	return tea.Tick(freezeQuitWait, func(time.Time) tea.Msg { return freezeTimeoutMsg{} })
}

// quitIfIdle quits when quitting and nothing is left to wait for.
func (m *Model) quitIfIdle() tea.Cmd {
	if m.quitting && len(m.cancels) == 0 && !m.preparing {
		return tea.Quit
	}
	return nil
}

func (m *Model) push(s tui.Screen) tea.Cmd {
	if s == nil {
		return nil
	}
	if sp, ok := m.top().(tui.Suspender); ok {
		sp.Suspend(m.ctx)
	}
	m.stack = append(m.stack, s)
	return s.Init(m.ctx)
}

func (m *Model) pop() tea.Cmd {
	if len(m.stack) == 1 {
		return nil
	}
	old := m.top()
	m.stack = m.stack[:len(m.stack)-1]
	if sp, ok := old.(tui.Suspender); ok {
		sp.Suspend(m.ctx)
	}
	if r, ok := m.top().(tui.Resumer); ok {
		return r.Resume(m.ctx)
	}
	return nil
}

func (m *Model) open(name string, args ...any) tea.Cmd {
	s := m.ctx.Open(name, args...)
	if s == nil {
		m.flash = tui.FlashMsg{Kind: '!', Text: "not available yet"}
		return nil
	}
	return m.push(s)
}

// View draws the top screen in the frame.
func (m *Model) View() string {
	top := m.top()
	w := max(0, m.ctx.Width-tui.ChromeCols)
	h := max(0, m.ctx.Height-tui.ChromeRows)
	// The body first: a screen may settle in View what its keys offer.
	body := top.View(m.ctx, w, h)
	f := tui.Footer{Flash: m.flash, Hints: top.Keys()}
	note := ""
	if n, ok := top.(tui.Noter); ok {
		note = n.Note(m.ctx)
		if f.Flash.Text == "" {
			f.Flash = n.Notice(m.ctx)
		}
	}
	if m.quitAsk != nil {
		f.Flash = tui.FlashMsg{Kind: '!', Text: m.quitAsk.View(m.ctx.Width)}
	}
	if m.quitting {
		f.Flash = tui.FlashMsg{Kind: '!', Text: "stopping runs…"}
	}
	return tui.Frame(m.ctx, top.Title(), note, body, f)
}

func (m *Model) fail(format string, args ...any) {
	m.flash = tui.FlashMsg{Kind: '✗', Text: fmt.Sprintf(format, args...)}
}

// --- loads ---

func (m *Model) startCatalog() tea.Cmd {
	m.d.Store.BeginLoad(store.CatalogKey)
	return loaders.Catalog(m.d.Client, m.d.Now)
}

// startInventory reads the variables of every environment of the given
// projects (those in the catalog), and the selected environments' configs.
func (m *Model) startInventory(projectIDs []string) tea.Cmd {
	sel := &settings.Settings{Projects: projectIDs, Envs: m.d.Settings.Envs}
	ts := loaders.LoadTargets(m.d.Store.Projects(), sel)
	for _, t := range ts {
		m.d.Store.BeginLoad(store.InventoryKey(t.ProjectID))
	}
	// Configs are read for the selected environments only: database
	// detection needs variables, not config.
	envIDs := loaders.EnvIDs(loaders.Targets(m.d.Store.Projects(), sel))
	for _, id := range envIDs {
		m.d.Store.BeginLoad(store.ConfigKey(id))
	}
	m.invSeq++
	return tea.Batch(
		loaders.Inventory(m.d.Client, ts, m.invSeq, m.d.Now),
		loaders.Config(m.d.Client, envIDs, m.d.Now),
	)
}

// --- saves ---

// saveJob takes a snapshot of the change set now and returns its write, for
// a command to run. Writes are serialised by the saver: a snapshot older
// than one already written is skipped (the newer one holds what it held),
// and the job reports the newer write's result: when that failed, what the
// older snapshot held is not on disk either.
func (m *Model) saveJob() func() error {
	m.saveSeq++
	seq, sv, path := m.saveSeq, m.saver, m.d.ChangeSetPath
	snap := m.d.CS.Clone()
	return func() error {
		sv.mu.Lock()
		defer sv.mu.Unlock()
		if seq < sv.last {
			return sv.lastErr
		}
		sv.last = seq
		sv.lastErr = nil
		if err := changeset.Save(path, snap); err != nil {
			sv.lastErr = fmt.Errorf("change set not saved: %w", err)
		}
		return sv.lastErr
	}
}

// saveCS writes a snapshot of the change set in the background.
func (m *Model) saveCS() tea.Cmd {
	save := m.saveJob()
	return func() tea.Msg {
		if err := save(); err != nil {
			return tui.FlashMsg{Kind: '✗', Text: err.Error()}
		}
		return nil
	}
}

func (m *Model) saveSettings() tea.Cmd {
	s := *m.d.Settings
	for _, l := range []*[]string{&s.Projects, &s.Envs, &s.WriteEnvs, &s.Internal} {
		*l = slices.Clone(*l)
	}
	// The --project/--env override is for this run only: an unedited
	// overridden list is saved as it was before.
	// Compared as sets: pickers hand ids back in catalog order.
	if m.sel.projects != nil && sameSet(s.Projects, m.sel.projects) {
		s.Projects = slices.Clone(m.sel.savedProjects)
	}
	if m.sel.envs != nil && sameSet(s.Envs, m.sel.envs) {
		s.Envs = slices.Clone(m.sel.savedEnvs)
	}
	path := m.d.SettingsPath
	return func() tea.Msg {
		if err := settings.Save(path, s); err != nil {
			return tui.FlashMsg{Kind: '✗', Text: "settings not saved: " + err.Error()}
		}
		return tui.FlashMsg{Kind: '✓', Text: "saved settings"}
	}
}

// sameSet reports whether a and b hold the same ids in any order.
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// --- change set intents ---

func (m *Model) put(c changeset.Change) tea.Cmd {
	done, err := m.tryPut(c)
	if err != nil {
		m.fail("%v", err)
		return nil
	}
	m.flash = tui.FlashMsg{Kind: '✓', Text: done}
	return m.saveCS()
}

// tryPut puts c in the change set; the text says what it did. Nothing
// changes on an error.
func (m *Model) tryPut(c changeset.Change) (string, error) {
	k, ok := m.d.Reg[c.Kind]
	if !ok {
		return "", fmt.Errorf("unknown change kind %s", c.Kind)
	}
	if err := k.Validate(m.d.Store, c); err != nil {
		return "", err
	}
	// A put that brings an existing change back to its base removes it: the
	// same guard as an explicit remove applies.
	if i := slices.IndexFunc(m.d.CS.Changes, func(o changeset.Change) bool { return o.Key() == c.Key() }); i >= 0 {
		old := m.d.CS.Changes[i]
		merged := c
		merged.Base = old.Base
		if old.RunID == "" && k.IsNoop(merged) && m.referenced(old) {
			return "", fmt.Errorf("%s is referenced by a pending change: remove that first", old.Target)
		}
	}
	if err := m.d.CS.Put(c, k.IsNoop); err != nil {
		return "", err
	}
	i := slices.IndexFunc(m.d.CS.Changes, func(o changeset.Change) bool { return o.Key() == c.Key() })
	if i < 0 {
		// c's copies go with it (an unknown ID has none).
		gone, _ := m.dropCopies(c.ID)
		return c.Target + " is back at its base: change removed" + copiesText(gone), nil
	}
	if n, err := m.followCopies(m.d.CS.Changes[i]); err != nil || n > 0 {
		return c.Target + " is in the change set · " + strconv.Itoa(n) + " copies follow", err
	}
	return c.Target + " is in the change set", nil
}

// followCopies sets the copies of src's planned value to the value src
// plans now; when src plans none (a delete), they are removed. It returns
// how many follow.
func (m *Model) followCopies(src changeset.Change) (int, error) {
	value, ok := kinds.SetValue(src)
	if !ok {
		_, err := m.dropCopies(src.ID)
		return 0, err
	}
	copies := kinds.CopiesOf(m.d.CS.Changes, src.ID)
	for _, c := range copies {
		f, err := kinds.Follow(c, value)
		if err != nil {
			return 0, err
		}
		if err := m.d.CS.Put(f, nil); err != nil {
			return 0, err
		}
	}
	return len(copies), nil
}

// dropCopies removes the copies of id's planned value (CopiesOf); a copy a
// run holds stops it before anything is removed. It returns how many went.
func (m *Model) dropCopies(id string) (int, error) {
	copies := kinds.CopiesOf(m.d.CS.Changes, id)
	for _, c := range copies {
		if c.RunID != "" {
			return 0, fmt.Errorf("a copy of it, %s, is in run %s", c.Target, c.RunID)
		}
	}
	for _, c := range copies {
		if err := m.d.CS.Remove(c.ID); err != nil {
			return 0, err
		}
	}
	return len(copies), nil
}

// copiesText is " · N copies too" for n removed copies; "" for none.
func copiesText(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return " · 1 copy of it too"
	}
	return " · " + strconv.Itoa(n) + " copies of it too"
}

// referenced reports whether another unlocked pending change reads what c
// writes.
func (m *Model) referenced(c changeset.Change) bool {
	writes := m.d.Reg.Writes(c)
	for _, o := range m.d.CS.Changes {
		if o.ID == c.ID || o.RunID != "" {
			continue
		}
		if slices.ContainsFunc(m.d.Reg.Reads(o), func(r string) bool { return slices.Contains(writes, r) }) {
			return true
		}
	}
	return false
}

// remove drops a change unless another pending change reads what it writes
// (B = ${{A}}): removing A would leave B pointing at nothing.
func (m *Model) remove(id string) tea.Cmd {
	done, err := m.tryRemove(id)
	if err != nil {
		m.fail("%v", err)
		return nil
	}
	m.flash = tui.FlashMsg{Kind: '✓', Text: done}
	return m.saveCS()
}

// tryRemove drops change id; the text says what it did. Nothing changes on
// an error.
func (m *Model) tryRemove(id string) (string, error) {
	c, ok := m.d.CS.Get(id)
	if !ok {
		return "", fmt.Errorf("change %s not found", id)
	}
	if m.referenced(c) {
		return "", fmt.Errorf("%s is referenced by a pending change: remove that first", c.Target)
	}
	// A database password change takes the changes its decision made by
	// itself with it; the places the user decided stay.
	auto, decided := kinds.PasswordFollowers(m.d.CS.Changes, c)
	for _, cp := range kinds.CopiesOf(m.d.CS.Changes, id) {
		if cp.RunID != "" {
			return "", fmt.Errorf("a copy of it, %s, is in run %s", cp.Target, cp.RunID)
		}
	}
	if err := m.d.CS.Remove(id); err != nil {
		return "", err
	}
	copies, err := m.dropCopies(id)
	if err != nil {
		return "", err
	}
	for _, a := range auto {
		if err := m.d.CS.Remove(a); err != nil {
			return "", err
		}
	}
	done := "removed " + c.Target + copiesText(copies)
	if decided > 0 {
		done += " · " + strconv.Itoa(decided) + " decided places kept · they no longer follow a password change"
	}
	return done, nil
}

// changes applies a ChangesMsg: the removes, then the puts, each checked as
// alone. Done is flashed when all succeed, else the first failure, with how
// many of the intents were applied when some were.
func (m *Model) changes(msg tui.ChangesMsg) tea.Cmd {
	var first error
	applied, total := 0, len(msg.Remove)+len(msg.Put)
	note := func(err error) {
		if err == nil {
			applied++
		} else if first == nil {
			first = err
		}
	}
	present := map[string]bool{}
	for _, id := range msg.Remove {
		if _, ok := m.d.CS.Get(id); ok {
			present[id] = true
		}
	}
	for _, id := range msg.Remove {
		if _, ok := m.d.CS.Get(id); !ok && present[id] {
			applied++ // went with the change it copies
			continue
		}
		_, err := m.tryRemove(id)
		note(err)
	}
	for _, c := range msg.Put {
		_, err := m.tryPut(c)
		note(err)
	}
	switch {
	case first == nil:
		kind := msg.Kind
		if kind == 0 {
			kind = '✓'
		}
		m.flash = tui.FlashMsg{Kind: kind, Text: text.Sanitize(msg.Done)}
	case applied > 0:
		m.fail("%v · %d of %d applied", first, applied, total)
	default:
		m.fail("%v", first)
	}
	if applied == 0 {
		return nil
	}
	return m.saveCS()
}

// --- runs ---

// conflictFlash reports the first conflict. With the unselected rest it
// says which way the dependency goes: the selection reads what an unselected
// change writes, or an unselected change reads what the selection writes.
func (m *Model) conflictFlash(cs []changeset.Conflict) {
	c := cs[0]
	if c.OtherRun == "" {
		if m.d.Reg != nil && !slices.Contains(m.d.Reg.Reads(c.Candidate), c.Target) &&
			slices.Contains(m.d.Reg.Reads(c.Other), c.Target) {
			m.fail("an unselected change needs %s: include it or remove it", m.targetLabel(c.Target))
			return
		}
		m.fail("needs an unselected change: %s", m.targetLabel(c.Target))
		return
	}
	m.fail("conflicts with run %s: %s", c.OtherRun, m.targetLabel(c.Target))
}

// targetLabel makes a conflict target readable, sanitized:
//
//	var:<env>/<service>/<name>      <project>/<env> <service>.<name>
//	limits:<env>/<service>          <project>/<env> <service> limits
//	volume:<env>/<volume>           <project>/<env> <service> volume <mount
//	path> domain:<env>/<service>/<target> <project>/<env> <service> domain
//	<host>
//
// with names where the store knows them; other targets are shown as they are.
func (m *Model) targetLabel(t string) string {
	return text.Sanitize(m.rawTargetLabel(t))
}

func (m *Model) rawTargetLabel(t string) string {
	st := m.d.Store
	svcName := func(id string) string {
		if id == "shared" {
			id = ""
		}
		return st.ServiceName(id)
	}
	kind, loc, ok := strings.Cut(t, ":")
	if !ok {
		return t
	}
	switch kind {
	case "var":
		parts := strings.SplitN(loc, "/", 3)
		if len(parts) != 3 {
			return t
		}
		return st.EnvLabel(parts[0]) + " " + svcName(parts[1]) + "." + parts[2]
	case "limits":
		env, svc, ok := strings.Cut(loc, "/")
		if !ok {
			return t
		}
		return st.EnvLabel(env) + " " + svcName(svc) + " limits"
	case "volume":
		env, vol, ok := strings.Cut(loc, "/")
		if !ok {
			return t
		}
		if vm, ok := st.VolumeMount(env, vol); ok {
			at := vm.MountPath
			if at == "" {
				at = vol
			}
			return st.EnvLabel(env) + " " + svcName(vm.ServiceID) + " volume " + at
		}
		return st.EnvLabel(env) + " volume " + vol
	case "domain":
		parts := strings.SplitN(loc, "/", 3)
		if len(parts) != 3 {
			return t
		}
		where := st.EnvLabel(parts[0]) + " " + svcName(parts[1])
		if host, ok := strings.CutPrefix(parts[2], "custom-domain:"); ok {
			return where + " domain " + host
		}
		id, ok := strings.CutPrefix(parts[2], "service-domain:")
		if !ok {
			return t
		}
		if id == "new" {
			return where + " new railway domain"
		}
		if d, ok := st.Domains(st.PlaceOf(parts[0], parts[1])); ok {
			for _, sd := range d.Service {
				if sd.ID == id && sd.Domain != "" {
					return where + " domain " + sd.Domain
				}
			}
		}
		return where + " railway domain " + id
	}
	return t
}

// envStepPrefixes start the step keys that name an environment, as
// "<prefix><env>" or "<prefix><env>/<service>".
var envStepPrefixes = []string{
	"patch:",
	"redeploy:",
	"restart:",
}

// writeRefused flashes and reports true when cs would patch or redeploy an
// environment outside Settings' write environments, so no run is created
// for a write the client would refuse. The envs are those of the changes
// (but markers, which write nothing) and of the steps cs compiles to
// against the snapshot.
func (m *Model) writeRefused(cs changeset.ChangeSet) bool {
	envs := map[string]bool{}
	for _, c := range cs.Changes {
		// A marker (variable.keep) writes nothing: it is allowed in a
		// read-only environment.
		if c.Place.EnvID != "" && !m.d.Reg.Marker(c) {
			envs[c.Place.EnvID] = true
		}
	}
	if m.d.Runs.Compile != nil {
		if g, err := m.d.Runs.CompileNow(cs); err == nil {
			for k := range g.Steps {
				for _, prefix := range envStepPrefixes {
					if rest, ok := strings.CutPrefix(k, prefix); ok {
						if env, _, _ := strings.Cut(rest, "/"); env != "" {
							envs[env] = true
						}
					}
				}
			}
		}
	}
	for _, env := range slices.Sorted(maps.Keys(envs)) {
		if !slices.Contains(m.d.Settings.WriteEnvs, env) {
			m.fail("%s is not in write environments · Settings w", m.d.Store.EnvLabel(env))
			return true
		}
	}
	return false
}

// selection is what apply(ids) would run: the named changes, or every
// unlocked valid one when ids is empty. It flashes and reports false when
// the apply is refused: a named change is invalid, missing or held by a
// run, nothing is left to apply, or an environment is not writable.
// Prepare checks all of it again under the prepare lock.
func (m *Model) selection(ids []string) (changeset.ChangeSet, bool) {
	for _, id := range ids {
		if _, ok := m.d.CS.Get(id); !ok {
			m.fail("change %s not found", text.Sanitize(id))
			return changeset.ChangeSet{}, false
		}
	}
	var sel changeset.ChangeSet
	sel.RedeployOff = slices.Clone(m.d.CS.RedeployOff)
	invalid := 0
	for _, c := range m.d.CS.Changes {
		named := slices.Contains(ids, c.ID)
		if !named && (len(ids) > 0 || c.RunID != "") {
			continue
		}
		if named && c.RunID != "" {
			m.fail("%s: %v", c.Key(), changeset.ErrLocked)
			return sel, false
		}
		if err := m.d.Reg.CheckData(c); err != nil {
			if named {
				m.fail("%s is invalid and cannot be applied: %v · x removes it", c.Target, err)
				return sel, false
			}
			invalid++
			continue
		}
		sel.Changes = append(sel.Changes, c)
	}
	switch {
	case len(ids) == 0 && len(sel.Changes) == 0 && invalid > 0:
		m.fail("nothing to apply: every unlocked change is invalid")
		return sel, false
	case len(sel.Changes) == 0:
		m.fail("no unlocked changes to apply")
		return sel, false
	}
	return sel, !m.writeRefused(sel)
}

// newRunID names the run a prepare freezes; a variable for tests.
var newRunID = apply.NewRunID

// marshalSelection encodes a selection for its fingerprint; a variable for
// tests.
var marshalSelection = json.Marshal

// fingerprint identifies a selection's content: its changes, values
// included, and the redeploy opt-outs. Only compared, never shown. A
// selection that does not encode has none (ok false): drift could not be
// told, so the apply is refused.
func fingerprint(sel changeset.ChangeSet) (sum [sha256.Size]byte, ok bool) {
	data, err := marshalSelection(sel)
	if err != nil {
		return sum, false
	}
	return sha256.Sum256(data), true
}

// unfingerprintable is the fixed flash of a selection fingerprint fails
// on; the encoder's error can quote values.
const unfingerprintable = "the change set cannot be checked for changes · not applied"

// A prepare runs its disk work in commands, one step at a time, with
// preparing set from the apply until the prepare lock is released:
//
//  1. lockPrepare takes the prepare lock (preparedMsg). prepared checks the
//     selection again and starts the check on a copy of the live set.
//  2. The check (PrepareLock.Check: conflicts and compile) writes nothing
//     (checkedMsg). checked refuses when the live set's Rev moved meanwhile,
//     else locks the selection in the live set under a new run id, in
//     memory only, and starts the freeze.
//  3. The freeze (PrepareLock.Freeze, then the lock released) writes the run
//     (frozenMsg). frozen saves the live set and executes the run, or
//     unlocks the selection when the freeze failed.
//
// A refused step releases the lock in a command (releasedMsg). An immediate
// change checks, freezes and releases in one command (immediateMsg).

// preparedMsg carries the prepare lock LockPrepare took off the UI
// goroutine, for an apply of ids (with the fingerprint of the selection
// confirmed) or, with immediate set, one change.
type preparedMsg struct {
	lock      *apply.PrepareLock
	err       error
	ids       []string
	sel       [sha256.Size]byte
	immediate *changeset.Change
}

// checkedMsg is the selection checked and compiled on a copy of the live
// set whose Rev was rev.
type checkedMsg struct {
	lock      *apply.PrepareLock
	rev       uint64
	plan      apply.Plan
	conflicts []changeset.Conflict
	err       error
}

// frozenMsg is the run runID frozen (err nil) or not, with the error of
// releasing the prepare lock after it. ids are the selection, flags its
// flags before the live set's Lock cleared them.
type frozenMsg struct {
	runID      string
	ids        []string
	flags      map[string]string
	err        error
	releaseErr error
}

// releasedMsg ends a prepare that froze no run.
type releasedMsg struct{ err error }

// immediateMsg is an immediate change prepared: the run, or its conflicts,
// or the error.
type immediateMsg struct {
	change    changeset.Change
	runID     string
	conflicts []changeset.Conflict
	err       error
}

// lockPrepare waits for the prepare lock in the command's goroutine: another
// instance can hold it for up to seconds. One prepare at a time.
func (m *Model) lockPrepare(msg preparedMsg) tea.Cmd {
	m.preparing = true
	runs := m.d.Runs
	return func() tea.Msg {
		msg.lock, msg.err = runs.LockPrepare()
		return msg
	}
}

// release releases a prepare lock that is not used, in a command; the
// prepare ends when it lands (released).
func release(l *apply.PrepareLock) tea.Cmd {
	return func() tea.Msg { return releasedMsg{err: l.Release()} }
}

// released ends the prepare, flashing a failure to release the lock.
func (m *Model) released(msg releasedMsg) tea.Cmd {
	m.preparing = false
	if msg.err != nil {
		m.fail("%v", msg.err)
	}
	m.revertNotStarted() // released without a run
	return m.quitIfIdle()
}

// revertNotStarted, when the prepare that ended without a run was a
// revert's (reverted), flashes that the revert is pending in the change set.
func (m *Model) revertNotStarted() {
	if m.revertID == "" {
		return
	}
	m.flash = tui.FlashMsg{Kind: '!', Text: "revert of " + m.revertLabel + " is pending · A applies it"}
	m.revertID, m.revertLabel = "", ""
}

// preparingRefused flashes and reports true while a prepare is pending.
func (m *Model) preparingRefused() bool {
	if m.preparing {
		m.flash = tui.FlashMsg{Kind: '!', Text: "a run is being prepared"}
	}
	return m.preparing
}

// apply starts a run of the changes ids, or of every unlocked valid change
// when ids is empty. An invalid change (Registry.CheckData, e.g. corrupt
// on disk) is never applied: naming one is refused with its reason. The
// prepare lock is taken off the UI goroutine; prepared goes on.
func (m *Model) apply(ids []string) tea.Cmd {
	if m.preparingRefused() {
		return nil
	}
	var uniq []string
	for _, id := range ids {
		if !slices.Contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	sel, ok := m.selection(uniq)
	if !ok {
		return nil
	}
	sum, ok := fingerprint(sel)
	if !ok {
		m.fail(unfingerprintable)
		return nil
	}
	return m.lockPrepare(preparedMsg{ids: uniq, sel: sum})
}

// prepared goes on under the lock it got. The live set may have changed
// while the lock was awaited: the selection is checked again, and one that
// is no longer what was confirmed (a change edited, added or removed, a
// redeploy toggled) is refused. Then the check runs in a command on a copy
// of the live set; Check(nil) selects exactly the confirmed changes.
// Quitting meanwhile releases the lock and quits.
func (m *Model) prepared(msg preparedMsg) tea.Cmd {
	if msg.err != nil {
		m.preparing = false
		m.fail("%v", msg.err)
		m.revertNotStarted()
		return m.quitIfIdle()
	}
	if m.quitting {
		return release(msg.lock)
	}
	if msg.immediate != nil {
		return m.prepareImmediate(msg.lock, *msg.immediate)
	}
	sel, ok := m.selection(msg.ids)
	if !ok {
		return release(msg.lock)
	}
	switch sum, ok := fingerprint(sel); {
	case !ok:
		m.fail(unfingerprintable)
		return release(msg.lock)
	case sum != msg.sel:
		m.fail("the change set changed while preparing · apply again")
		return release(msg.lock)
	}
	snap, lock, ids := m.d.CS.Clone(), msg.lock, msg.ids
	return func() tea.Msg {
		plan, conflicts, err := lock.Check(snap, ids)
		return checkedMsg{lock: lock, rev: snap.Rev, plan: plan, conflicts: conflicts, err: err}
	}
}

// checked locks the checked selection in the live set under a new run id
// and freezes it in a command. A live set changed since its copy was taken
// is refused: the check may have missed a conflict with what changed.
// The freeze command first saves the live set holding the lock (saveJob)
// and writes the run only once that save (or a newer one) succeeded, so no
// run exists on disk whose changes are not held there. A failed save or
// freeze is undone by frozen, which unlocks the selection; a crash after
// the save but before the run is whole is undone by the startup unlock (the
// run is gone or has no journal).
func (m *Model) checked(msg checkedMsg) tea.Cmd {
	switch {
	case m.quitting:
		return release(msg.lock)
	case m.d.CS.Rev != msg.rev:
		m.fail("the change set changed while preparing · apply again")
		return release(msg.lock)
	case msg.err != nil:
		m.fail("%v", msg.err)
		return release(msg.lock)
	case len(msg.conflicts) > 0:
		m.conflictFlash(msg.conflicts)
		return release(msg.lock)
	case m.writeRefused(msg.plan.Set): // the write environments may have changed
		return release(msg.lock)
	}
	flags := map[string]string{}
	for _, id := range msg.plan.IDs {
		if c, ok := m.d.CS.Get(id); ok {
			flags[id] = c.Flag
		}
	}
	runID := newRunID()
	for m.runIDTaken(runID) {
		runID = newRunID()
	}
	if err := m.d.CS.Lock(msg.plan.IDs, runID); err != nil {
		m.fail("%v", err)
		return release(msg.lock)
	}
	m.freezing = runID
	// The live set holding the run is saved before the run is written, so
	// no run exists on disk whose changes are not held there.
	save, lock, plan := m.saveJob(), msg.lock, msg.plan
	return func() tea.Msg {
		err := save()
		if err == nil {
			err = lock.Freeze(plan, runID)
		}
		return frozenMsg{runID: runID, ids: plan.IDs, flags: flags, err: err, releaseErr: lock.Release()}
	}
}

// runIDTaken reports whether a live change or a run of this process already
// carries the run id.
func (m *Model) runIDTaken(id string) bool {
	if _, ok := m.cancels[id]; ok {
		return true
	}
	if _, ok := m.ctx.Running[id]; ok {
		return true
	}
	return slices.ContainsFunc(m.d.CS.Changes, func(c changeset.Change) bool { return c.RunID == id })
}

// frozen saves the live set holding the new run and then executes it. A
// failed freeze (or save before it) unlocks the selection, and only it, with
// its old flags. Quitting meanwhile saves
// the run's changes as held, so it can be resumed, and quits.
func (m *Model) frozen(msg frozenMsg) tea.Cmd {
	m.preparing, m.freezing = false, ""
	runID := msg.runID
	if msg.err != nil {
		m.d.CS.UnlockChanges(runID, msg.ids, msg.flags)
		m.fail("%v", errors.Join(msg.err, msg.releaseErr))
		m.revertNotStarted()
		return m.saveThenQuitIfIdle()
	}
	m.revertID, m.revertLabel = "", "" // started
	if msg.releaseErr != nil {
		// The run exists and holds its changes; only releasing the prepare
		// lock failed: run it anyway; the flash keeps the error.
		m.fail("%v", msg.releaseErr)
	}
	if m.quitting {
		return m.saveThenQuitIfIdle()
	}
	var frozen changeset.ChangeSet
	frozen.RedeployOff = slices.Clone(m.d.CS.RedeployOff)
	for _, c := range m.d.CS.Changes {
		if c.RunID == runID {
			frozen.Changes = append(frozen.Changes, c)
		}
	}
	m.track(runID, frozen, nil)
	return tea.Sequence(m.saveCS(), m.execute(runID, false))
}

// saveThenQuitIfIdle saves the live set and then quits when quitting and
// nothing is left to wait for.
func (m *Model) saveThenQuitIfIdle() tea.Cmd {
	save := m.saveCS()
	if q := m.quitIfIdle(); q != nil {
		return tea.Sequence(save, q)
	}
	return save
}

// immediate runs one change at once without touching the live set.
func (m *Model) immediate(c changeset.Change) tea.Cmd {
	if m.preparingRefused() || !m.immediateOK(c) {
		return nil
	}
	return m.lockPrepare(preparedMsg{immediate: &c})
}

// immediateOK flashes and reports false when an immediate change is refused:
// unknown kind, invalid against the store, or an environment not writable.
func (m *Model) immediateOK(c changeset.Change) bool {
	k, ok := m.d.Reg[c.Kind]
	if !ok {
		m.fail("unknown change kind %s", c.Kind)
		return false
	}
	if err := k.Validate(m.d.Store, c); err != nil {
		m.fail("%v", err)
		return false
	}
	return !m.writeRefused(changeset.ChangeSet{Changes: []changeset.Change{c}})
}

// prepareImmediate checks the change again after the wait, then checks,
// freezes and releases the lock in one command.
func (m *Model) prepareImmediate(lock *apply.PrepareLock, c changeset.Change) tea.Cmd {
	if !m.immediateOK(c) {
		return release(lock)
	}
	return func() tea.Msg {
		runID, conflicts, err := lock.PrepareImmediate(c)
		return immediateMsg{change: c, runID: runID, conflicts: conflicts, err: err}
	}
}

// immediatePrepared executes the prepared immediate run. A run created
// whose prepare lock failed to release runs anyway, with the error flashed;
// quitting leaves it to be resumed.
func (m *Model) immediatePrepared(msg immediateMsg) tea.Cmd {
	m.preparing = false
	switch {
	case len(msg.conflicts) > 0:
		m.conflictFlash(msg.conflicts)
		return m.quitIfIdle()
	case msg.err != nil:
		m.fail("%v", msg.err)
		if msg.runID == "" {
			return m.quitIfIdle()
		}
	}
	if m.quitting {
		return m.quitIfIdle()
	}
	m.track(msg.runID, changeset.ChangeSet{Changes: []changeset.Change{msg.change}}, nil)
	return m.execute(msg.runID, true)
}

// resumeReadMsg is a run to resume as read off the UI goroutine: its frozen
// change set and journaled steps.
type resumeReadMsg struct {
	runID string
	cs    changeset.ChangeSet
	prior map[string]apply.StepRecord
	found bool
	err   error // the run is corrupt
}

// resume reads the run in the command's goroutine; resumeRead starts it.
func (m *Model) resume(runID string) tea.Cmd {
	if _, ok := m.cancels[runID]; ok {
		m.flash = tui.FlashMsg{Kind: '!', Text: "run " + runID + " is already applying"}
		return nil
	}
	if m.busyRefused(runID) {
		return nil
	}
	runs := m.d.Runs
	return func() tea.Msg {
		msg := resumeReadMsg{runID: runID}
		un, errs := runs.Unfinished()
		msg.cs, msg.found = un[runID]
		if !msg.found {
			for _, err := range errs {
				var re *apply.RunError
				if errors.As(err, &re) && re.RunID == runID {
					msg.err = err
				}
			}
			return msg
		}
		infos, _ := runs.List()
		for _, in := range infos {
			if in.ID == runID {
				msg.prior = in.Steps
			}
		}
		return msg
	}
}

func (m *Model) resumeRead(msg resumeReadMsg) tea.Cmd {
	runID := msg.runID
	switch _, applying := m.cancels[runID]; {
	case applying:
		m.flash = tui.FlashMsg{Kind: '!', Text: "run " + runID + " is already applying"}
		return nil
	case m.quitting, m.busyRefused(runID):
		return nil
	case msg.err != nil:
		m.fail("%v", msg.err)
		return nil
	case !msg.found:
		m.fail("run %s is not unfinished", runID)
		return nil
	}
	m.track(runID, msg.cs, msg.prior)
	return m.execute(runID, false)
}

// busyRefused flashes and reports true when the run is being abandoned or
// frozen by this process.
func (m *Model) busyRefused(runID string) bool {
	switch {
	case m.abandoning[runID]:
		m.flash = tui.FlashMsg{Kind: '!', Text: "run " + runID + " is being abandoned"}
	case runID != "" && runID == m.freezing:
		m.flash = tui.FlashMsg{Kind: '!', Text: "run " + runID + " is being prepared"}
	default:
		return false
	}
	return true
}

// abandonedMsg is the result of a run's Abandon.
type abandonedMsg struct {
	runID string
	err   error
}

// abandon marks the run abandoned in a command; abandoned unlocks its
// changes. Until then the run is not resumed or abandoned again.
func (m *Model) abandon(runID string) tea.Cmd {
	if _, ok := m.cancels[runID]; ok {
		m.fail("run %s is applying", runID)
		return nil
	}
	if m.busyRefused(runID) {
		return nil
	}
	m.abandoning[runID] = true
	runs := m.d.Runs
	return func() tea.Msg { return abandonedMsg{runID: runID, err: runs.Abandon(runID)} }
}

// abandoned unlocks the abandoned run's changes, unless the run is applying
// now, and saves.
func (m *Model) abandoned(msg abandonedMsg) tea.Cmd {
	runID := msg.runID
	delete(m.abandoning, runID)
	if msg.err != nil {
		m.failRun(runID, msg.err)
		return nil
	}
	m.flash = tui.FlashMsg{Kind: '✓', Text: "abandoned run " + runID}
	if _, running := m.cancels[runID]; running {
		// Defensive only: abandon and resume refuse each other while the
		// abandon is pending, so the run cannot be applying here.
		return m.refreshRuns()
	}
	m.d.CS.Unlock(runID, nil)
	delete(m.ctx.Running, runID)
	return tea.Batch(m.saveCS(), m.refreshRuns())
}

// failRun flashes a run's abandon error; a lock another live instance
// holds reads "running in another window".
func (m *Model) failRun(runID string, err error) {
	var le *workdir.LockedError
	if errors.As(err, &le) {
		m.flash = tui.FlashMsg{Kind: '!', Text: tui.RunElsewhere(m.d.Runs.Dir, runID, le.PID)}
		return
	}
	m.fail("%v", err)
}

// revertReadMsg is the password a stalled run set, read for its revert.
type revertReadMsg struct {
	runID string
	row   rotation.PasswordRow
	login string // the password the run set; a secret
	err   error
}

// revertedMsg is the result of a revert's abandon.
type revertedMsg struct {
	runID  string
	row    rotation.PasswordRow
	change changeset.Change // the revert, checked before the abandon
	err    error            // the run was not abandoned
}

// revert reverts a stalled root password. In a
// command it reads the password the run set from its frozen set; then
// (revertRead) the revert change is built and checked against the change
// set and Vet before anything happens; only then the run is abandoned (no
// typed word) in a command, and reverted drops the run's changes of the
// row's group from the live set, puts the revert and starts its run. The
// run is guarded as an abandon from the start.
func (m *Model) revert(msg tui.RevertMsg) tea.Cmd {
	runID := msg.RunID
	if _, ok := m.cancels[runID]; ok {
		m.fail("run %s is applying", runID)
		return nil
	}
	if m.busyRefused(runID) {
		return nil
	}
	if m.preparing {
		// It would start at once after the abandon: not while another
		// run is being prepared.
		m.flash = tui.FlashMsg{Kind: '!', Text: "a run is being prepared · R again after it"}
		return nil
	}
	m.abandoning[runID] = true
	runs, row := m.d.Runs, msg.Row
	return func() tea.Msg {
		out := revertReadMsg{runID: runID, row: row}
		cs, err := runs.Frozen(runID)
		if err != nil {
			out.err = fmt.Errorf("cannot revert: run %s: frozen change set is unreadable", runID)
			return out
		}
		for _, c := range cs.Changes {
			if c.Group != row.Key {
				continue
			}
			if src, v, ok := kinds.PasswordValue(c); ok && src != kinds.SourceRevert {
				out.login = v
			}
		}
		if out.login == "" {
			out.err = fmt.Errorf("cannot revert: run %s set no password of %s", runID, row.Key)
		}
		return out
	}
}

// revertRead builds the revert and checks the change set would take it once
// the run's group left it; only then it abandons the run in a command.
// Refused, the run is left as it is.
func (m *Model) revertRead(msg revertReadMsg) tea.Cmd {
	runID := msg.runID
	refuse := func(err error) tea.Cmd {
		delete(m.abandoning, runID)
		m.flash = tui.FlashMsg{Kind: '✗', Text: text.Sanitize(err.Error())}
		return m.refreshRuns()
	}
	if msg.err != nil {
		return refuse(msg.err)
	}
	c, err := kinds.DBPasswordRevert(m.d.Store, msg.row, msg.login, m.d.Now())
	if err == nil {
		err = m.revertAccepted(c, runID, msg.row.Key)
	}
	if err != nil {
		return refuse(fmt.Errorf("cannot revert: %w", err))
	}
	runs, row := m.d.Runs, msg.row
	return func() tea.Msg { return revertedMsg{runID: runID, row: row, change: c, err: runs.Abandon(runID)} }
}

// revertAccepted checks, on a copy of the change set, that the revert c is
// put once the run is unlocked and the group's changes are dropped.
func (m *Model) revertAccepted(c changeset.Change, runID, group string) error {
	k, ok := m.d.Reg[c.Kind]
	if !ok {
		return fmt.Errorf("unknown change kind %s", c.Kind)
	}
	if err := k.Validate(m.d.Store, c); err != nil {
		return err
	}
	cp := m.d.CS.Clone()
	cp.Unlock(runID, nil)
	cp.Drop(groupIDs(cp.Changes, group))
	if err := cp.Put(c, k.IsNoop); err != nil {
		return err
	}
	if _, ok := cp.Get(c.ID); !ok {
		return errors.New("the database already holds that password")
	}
	// The revert runs at once after the abandon: what its prepare would
	// refuse is refused now, while the run can still be continued.
	return m.d.Runs.VetNow(changeset.ChangeSet{Changes: []changeset.Change{c}, RedeployOff: cp.RedeployOff}, cp)
}

// groupIDs are the ids of the changes of group.
func groupIDs(cl []changeset.Change, group string) []string {
	var out []string
	for _, c := range cl {
		if c.Group == group {
			out = append(out, c.ID)
		}
	}
	return out
}

// reverted finishes a revert after the abandon: the run's changes are
// unlocked, those of the row's group (the old password, its rewrites and
// decided places, locked or not) leave the live set in this same update,
// the checked revert is put, the set is saved, and a run of the revert
// alone starts at once: it is not left pending.
func (m *Model) reverted(msg revertedMsg) tea.Cmd {
	runID := msg.runID
	delete(m.abandoning, runID)
	if msg.err != nil {
		m.failRun(runID, msg.err)
		return m.refreshRuns()
	}
	delete(m.ctx.Running, runID)
	m.d.CS.Unlock(runID, nil)
	m.d.CS.Drop(groupIDs(m.d.CS.Changes, msg.row.Key))
	if _, err := m.tryPut(msg.change); err != nil {
		m.flash = tui.FlashMsg{Kind: '✗', Text: "abandoned run " + runID + " · cannot revert: " + text.Sanitize(err.Error())}
		return tea.Batch(m.saveCS(), m.refreshRuns())
	}
	m.flash = tui.FlashMsg{Kind: '✓', Text: "abandoned run " + runID + " · reverting the database password"}
	m.revertID, m.revertLabel = msg.change.ID, text.Sanitize(m.d.Store.ServiceName(msg.row.DB.Owner.ServiceID))+" root"
	start := m.apply([]string{msg.change.ID})
	if start == nil {
		m.revertNotStarted()
	}
	return tea.Batch(m.saveCS(), m.refreshRuns(), start)
}

// refreshRuns reads the runs index off the UI goroutine. Reads coalesce:
// while one is in flight, any number of calls queue one more.
func (m *Model) refreshRuns() tea.Cmd {
	if m.d.Runs == nil || m.quitting {
		return nil
	}
	if m.runsInFlight {
		m.runsQueued = true
		return nil
	}
	m.runsInFlight = true
	return loaders.Runs(m.d.Runs)
}

// runsRead stores the index and starts the queued read. Skipped runs are
// flashed when they change.
func (m *Model) runsRead(msg loaders.RunsMsg) tea.Cmd {
	m.runsInFlight = false
	m.ctx.RunIndex = tui.RunIndex{Loaded: true, Runs: msg.Runs, Warnings: msg.Warnings, Corrupt: msg.Corrupt}
	if w := strings.Join(msg.Warnings, "\n"); w != m.runsWarned {
		m.runsWarned = w
		switch {
		case len(msg.Warnings) == 0:
		case len(msg.Corrupt) == 0:
			m.fail("%s", msg.Warnings[0]) // the runs directory is unreadable
		default:
			t := msg.Warnings[0]
			if n := len(msg.Warnings) - 1; n > 0 {
				t += fmt.Sprintf(" (+%d more)", n)
			}
			m.flash = tui.FlashMsg{Kind: '!', Text: "skipped " + t + " · " + tui.RunRecovery(m.d.Runs.Dir, msg.Corrupt[0])}
		}
	}
	if !m.runsQueued {
		return nil
	}
	m.runsQueued = false
	return m.refreshRuns()
}

// track starts the progress of a run: its compiled steps, with the statuses
// a resumed run already journaled. Runs.Compile reads the store snapshot.
func (m *Model) track(runID string, cs changeset.ChangeSet, prior map[string]apply.StepRecord) {
	st := map[string]string{}
	if m.d.Runs.Compile != nil {
		if g, err := m.d.Runs.CompileNow(cs); err == nil {
			for k := range g.Steps {
				st[k] = ""
			}
		}
	}
	for k, rec := range prior {
		st[k] = rec.Status
	}
	m.steps[runID] = st
	delete(m.firstErr, runID)
	var projects []string
	for _, c := range cs.Changes {
		if c.Place.ProjectID != "" && !slices.Contains(projects, c.Place.ProjectID) {
			projects = append(projects, c.Place.ProjectID)
		}
	}
	m.runProjects[runID] = projects
	m.ctx.Running[runID] = tui.RunProgress{State: apply.RunRunning}
	m.progress(runID)
}

func (m *Model) progress(runID string) {
	p := m.ctx.Running[runID]
	p.Done, p.Total = 0, len(m.steps[runID])
	for _, s := range m.steps[runID] {
		if s == apply.StatusDone || s == apply.StatusSkipped {
			p.Done++
		}
	}
	m.ctx.Running[runID] = p
}

func (m *Model) event(e apply.Event) {
	if m.steps[e.RunID] == nil {
		m.steps[e.RunID] = map[string]string{}
	}
	m.steps[e.RunID][e.StepKey] = e.Status
	p, ok := m.ctx.Running[e.RunID]
	if !ok {
		p.State = apply.RunRunning
	}
	if e.Error != "" {
		if _, ok := m.firstErr[e.RunID]; !ok {
			m.firstErr[e.RunID] = e.Error
		}
		if p.Errors == nil {
			p.Errors = map[string]string{}
		}
		p.Errors[e.StepKey] = e.Error
	}
	m.ctx.Running[e.RunID] = p
	m.progress(e.RunID)
}

// execute runs Execute in the command's goroutine. Events go into an
// unbounded queue that one goroutine per run forwards through Send, so the
// executor never waits on the UI and events keep their order. The command
// returns RunDoneMsg after the queue is drained, so it arrives last.
func (m *Model) execute(runID string, immediate bool) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[runID] = cancel
	runs, send := m.d.Runs, m.d.Send
	return func() tea.Msg {
		defer cancel()
		q := &eventQueue{wake: make(chan struct{}, 1)}
		done := make(chan struct{})
		go func() {
			defer close(done)
			q.forward(send)
		}()
		rep, err := runs.Execute(ctx, runID, q.push)
		q.close()
		<-done
		return tui.RunDoneMsg{RunID: runID, Report: rep, Err: err, Immediate: immediate}
	}
}

// eventQueue is an unbounded FIFO of run events with one reader.
type eventQueue struct {
	mu     sync.Mutex
	items  []apply.Event
	closed bool
	wake   chan struct{} // capacity 1
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) push(e apply.Event) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	q.signal()
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

// forward sends every event in order until the queue is closed and empty.
func (q *eventQueue) forward(send func(tea.Msg)) {
	for {
		q.mu.Lock()
		batch, closed := q.items, q.closed
		q.items = nil
		q.mu.Unlock()
		for _, e := range batch {
			send(tui.RunEventMsg{Event: e})
		}
		if len(batch) == 0 {
			if closed {
				return
			}
			<-q.wake
		}
	}
}

// runDone reconciles the live set (only when the run reached a state),
// re-reads the run's projects and reports the result.
func (m *Model) runDone(msg tui.RunDoneMsg) tea.Cmd {
	id, rep, err := msg.RunID, msg.Report, msg.Err
	delete(m.cancels, id)
	projects := m.runProjects[id]
	delete(m.runProjects, id)
	stepErr := m.firstErr[id]
	delete(m.firstErr, id)
	if rep.State == "" {
		delete(m.ctx.Running, id)
		delete(m.steps, id)
		if err != nil && !errors.Is(err, context.Canceled) {
			m.fail("run %s failed: %s", id, firstLine(err.Error()))
		}
		return nil
	}
	p := m.ctx.Running[id]
	p.State = rep.State
	m.ctx.Running[id] = p

	var cmds []tea.Cmd
	if !msg.Immediate {
		apply.Reconcile(m.d.CS, id, rep)
		cmds = append(cmds, m.saveCS())
	}
	var wna *railway.WriteNotAllowedError
	switch {
	case errors.As(err, &wna):
		m.fail("%s is not in write environments · Settings w", m.d.Store.EnvLabel(wna.EnvID))
	case stepErr != "" && (err != nil || rep.State != apply.RunFinished):
		m.fail("run %s failed: %s", id, firstLine(stepErr))
	case err != nil:
		m.fail("run %s failed: %s", id, firstLine(err.Error()))
	case rep.State != apply.RunFinished:
		m.fail("run %s failed: %s", id, firstKept(rep))
	default:
		m.flash = tui.FlashMsg{Kind: '✓', Text: fmt.Sprintf("applied %d changes", len(rep.Done))}
	}
	if len(projects) > 0 && !m.quitting { // no reads while quitting: they would delay it
		cmds = append(cmds, m.startInventory(projects))
	}
	return tea.Batch(cmds...)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// firstKept is the first flag of a kept change, by change id.
func firstKept(rep apply.Report) string {
	for _, id := range slices.Sorted(maps.Keys(rep.Kept)) {
		if f := rep.Kept[id]; f != "" {
			return f
		}
	}
	return "not finished"
}
