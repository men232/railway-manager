package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/dbops"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/root"
	csscreen "github.com/men232/railway-manager/internal/tui/screens/changeset"
	"github.com/men232/railway-manager/internal/tui/screens/domains"
	"github.com/men232/railway-manager/internal/tui/screens/history"
	"github.com/men232/railway-manager/internal/tui/screens/logs"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
	"github.com/men232/railway-manager/internal/tui/screens/rotate"
	runscreen "github.com/men232/railway-manager/internal/tui/screens/run"
	"github.com/men232/railway-manager/internal/tui/screens/services"
	tuisettings "github.com/men232/railway-manager/internal/tui/screens/settings"
	"github.com/men232/railway-manager/internal/tui/screens/variables"
	"github.com/men232/railway-manager/internal/view"
	"github.com/men232/railway-manager/internal/workdir"
)

// executorConcurrency is how many steps run at once (global constraints).
const executorConcurrency = 6

// Options is what the program is built from. Main fills it from the real
// environment; tests point it at a temp home and a fake server.
type Options struct {
	Home     string              // the user's home directory
	Getenv   func(string) string // nil: os.Getenv
	Endpoint string              // Railway GraphQL endpoint
	// LiveEndpoint is the subscription websocket; empty means
	// railway.LiveEndpoint.
	LiveEndpoint string
	// Projects and Envs are the --project and --env values.
	Projects, Envs []string
	// FontDirs are scanned for a Nerd Font; nil means the platform's font
	// directories under Home.
	FontDirs []string
	// Refresh renews a config token; nil means `railway whoami`.
	Refresh func(context.Context) error
	// Program are extra tea.ProgramOptions (tests disable the terminal).
	Program []tea.ProgramOption
}

// screens is the registry the root opens screens from by name. The root
// adds "keys" itself.
func screens() map[string]func(args ...any) tui.Screen {
	return map[string]func(args ...any) tui.Screen{
		"services":      services.Build,
		"variables":     variables.Build,
		"history":       history.Build,
		"domains":       domains.Build,
		"logs":          logs.Build,
		"changeset":     csscreen.Build,
		"run":           runscreen.Build,
		"picker":        picker.Build,
		"rotate":        rotate.Build,
		"rotate.places": rotate.BuildPlaces,
		"settings":      func(...any) tui.Screen { return tuisettings.New() },
	}
}

// build wires the root model and the program that runs it.
func build(o Options) (*root.Model, *tea.Program, error) {
	d, err := wire(o)
	if err != nil {
		return nil, nil, err
	}
	var program *tea.Program
	d.Send = func(msg tea.Msg) { program.Send(msg) }
	m := root.New(d)
	program = tea.NewProgram(m, append([]tea.ProgramOption{tea.WithAltScreen()}, o.Program...)...)
	return m, program, nil
}

// runProgram runs p until it ends, however it ends, and then closes live:
// its socket and goroutines are gone when runProgram returns.
func runProgram(p *tea.Program, live *railway.Live) error {
	defer live.Close()
	_, err := p.Run()
	return err
}

// wire builds the root's dependencies, all but Send.
func wire(o Options) (root.Deps, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	now := time.Now

	work := workdir.Dir(getenv, o.Home)
	if err := workdir.Ensure(work); err != nil {
		return root.Deps{}, fmt.Errorf("work dir %s: %w", work, err)
	}
	// Best effort: the work dir holds secret values until they are applied.
	_ = workdir.Private(work)
	_, _ = workdir.CleanTemps(work, time.Minute, now())
	settingsPath := filepath.Join(work, "settings.json")
	set, err := settings.Load(settingsPath)
	if err != nil {
		return root.Deps{}, fmt.Errorf("settings %s: %w", settingsPath, err)
	}
	csPath := filepath.Join(work, "changeset.json")
	cs, err := changeset.Load(csPath)
	var note string
	if ce := (*changeset.CorruptError)(nil); errors.As(err, &ce) {
		note = "change set was unreadable, moved to " + ce.MovedTo + " · it holds secret values: delete it once you are done (it goes by itself after 30 days)"
	} else if err != nil {
		return root.Deps{}, fmt.Errorf("change set %s: %w", csPath, err)
	}

	refresh := o.Refresh
	if refresh == nil {
		refresh = proc.RailwayWhoami
	}
	tokens := railway.NewTokenSource(getenv, filepath.Join(o.Home, ".railway", "config.json"), refresh, now)
	writeEnvs := &atomic.Pointer[map[string]bool]{}
	client := &railway.Client{
		T: &railway.Transport{Endpoint: o.Endpoint, Tokens: tokens},
		WriteAllowed: func(envID string) bool {
			w := writeEnvs.Load()
			return w != nil && (*w)[envID]
		},
	}

	liveURL := o.LiveEndpoint
	if liveURL == "" {
		liveURL = railway.LiveEndpoint
	}
	// One socket for every subscription, on the client's token source. It
	// stays idle (no dial) until a screen subscribes; runProgram closes it.
	live := railway.NewLive(liveURL, tokens, nil)

	st := store.New()
	snapshot := &atomic.Pointer[store.Store]{}
	reg := kinds.NewRegistry()
	// A loaded change was never validated: judge each on its own data now
	// (nothing is read from Railway yet). Invalid ones stay in the set,
	// marked by the change set view, and are never applied.
	if n := invalidChanges(reg, cs); n > 0 {
		note = strconv.Itoa(n) + " changes in the change set are invalid and will not be applied · c change set"
		if n == 1 {
			note = "1 change in the change set is invalid and will not be applied · c change set"
		}
	}
	runs := &apply.Runs{
		Dir:  work, // runs live in <work>/runs
		Reg:  reg,
		Exec: &apply.Executor{Concurrency: executorConcurrency},
		NewEnv: func(runID string, j *apply.Journal) *apply.Env {
			return &apply.Env{
				API: client, Store: snapshot.Load(), Journal: j, RunID: runID,
				Proc: proc.OS{}, KnownHosts: dbops.KnownHostsFile(work), Sleep: sleep, Now: now,
			}
		},
		NewRedeploy:   kinds.NewRedeployStep,
		Fingerprint:   kinds.PasswordFingerprint,
		KeepAbandoned: kinds.RevertKeeps,
		Snapshot:      snapshot.Load,
		Compile:       compileWith(reg),
		// The write environments the client's WriteAllowed reads.
		Vet: func(s *store.Store, sel, live changeset.ChangeSet, actions []apply.FrozenAction) error {
			var write []string
			if w := writeEnvs.Load(); w != nil {
				for id, ok := range *w {
					if ok {
						write = append(write, id)
					}
				}
			}
			return kinds.CheckPasswordGroups(s, write, sel, live, actions)
		},
	}
	// Old runs go, with what is left of their values, and so do old
	// quarantined change sets.
	_, _ = runs.Prune(runRetention, now())
	_, _ = changeset.PruneCorrupt(csPath, runRetention, now())
	views := &view.Views{Store: st, Reg: reg, Compile: compileOn(reg, func() *store.Store { return st })}

	dirs := o.FontDirs
	if dirs == nil {
		dirs = fontDirs(runtime.GOOS, o.Home)
	}
	nerd := nerdFontInstalled(dirs)

	return root.Deps{
		Client: client, Live: live, Store: st, Views: views, Reg: reg, Runs: runs,
		Settings: &set, SettingsPath: settingsPath, ChangeSetPath: csPath,
		Snapshot: snapshot, WriteEnvs: writeEnvs,
		CS: &cs, LoadNote: note, Now: now,
		Screens:      screens(),
		Theme:        tui.NewTheme(lipgloss.ColorProfile()),
		Icons:        tui.Icons{Nerd: set.Icons == "nerd" || set.Icons == "auto" && nerd},
		IsTTY:        isTerminal(os.Stdout),
		NerdDetected: nerd,
		// Peek, not Get: TokenInfo runs on the UI goroutine and must never
		// start a refresh subprocess.
		TokenInfo:      func() string { return tokenInfo(tokens.Peek(), now()) },
		SelectProjects: names(o.Projects),
		SelectEnvs:     names(o.Envs),
	}, nil
}

// invalidChanges counts the changes of cs that reg.CheckData refuses.
func invalidChanges(reg apply.Registry, cs changeset.ChangeSet) int {
	n := 0
	for _, c := range cs.Changes {
		if reg.CheckData(c) != nil {
			n++
		}
	}
	return n
}

// compileOn compiles a change set against the store get returns.
func compileOn(reg apply.Registry, get func() *store.Store) func(changeset.ChangeSet) (apply.Graph, error) {
	compile := compileWith(reg)
	return func(cs changeset.ChangeSet) (apply.Graph, error) { return compile(get(), cs) }
}

// compileWith compiles a change set against the store it is given.
func compileWith(reg apply.Registry) func(*store.Store, changeset.ChangeSet) (apply.Graph, error) {
	return func(s *store.Store, cs changeset.ChangeSet) (apply.Graph, error) {
		return apply.Compile(cs, s, reg,
			func(envID string, changes []changeset.Change) apply.Step {
				// Compile groups the changes of one environment, so they
				// share its project.
				return kinds.NewPatchStep(envID, changes, reg, changes[0].Place.ProjectID)
			},
			kinds.NewRedeployStep)
	}
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// names trims the comma-split flag values and drops empty ones.
func names(vals []string) []string {
	var out []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// tokenInfo describes where the token comes from and when it expires, never
// the token itself.
func tokenInfo(st railway.TokenStatus, now time.Time) string {
	if st.Missing != "" {
		return "none: " + st.Missing
	}
	s := st.Source
	if !st.ExpiresAt.IsZero() {
		at := st.ExpiresAt.In(now.Location())
		layout := "15:04"
		if y, m, d := at.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
			layout = "2006-01-02 15:04"
		}
		s += " · expires " + at.Format(layout)
	}
	if st.RefreshErr != "" {
		s += " · refresh failed"
	}
	return s
}

// fontDirs lists where fonts are installed on goos.
func fontDirs(goos, home string) []string {
	if goos == "darwin" {
		return []string{
			filepath.Join(home, "Library", "Fonts"),
			"/Library/Fonts",
			"/System/Library/Fonts",
		}
	}
	return []string{
		filepath.Join(home, ".local", "share", "fonts"),
		filepath.Join(home, ".fonts"),
		"/usr/share/fonts",
	}
}

// nerdFontInstalled reports whether a file whose name contains "Nerd" is
// anywhere under dirs. Unreadable or missing directories are skipped.
func nerdFontInstalled(dirs []string) bool {
	for _, dir := range dirs {
		found := false
		_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
			if err != nil {
				if e != nil && e.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !e.IsDir() && strings.Contains(e.Name(), "Nerd") {
				found = true
				return fs.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

// isTerminal reports whether f is a character device (a terminal).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// runRetention is how long a finished or abandoned run's journal is kept.
const runRetention = 30 * 24 * time.Hour
