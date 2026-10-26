package tui

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/view"
)

// KeyHint is one footer hint. Always hints are shown first and never dropped;
// the rest are shown by ascending Rank while they fit.
type KeyHint struct {
	Key, Label string
	Always     bool
	// Optional hints are shown only in the room left after every other hint
	// (and "? more") is placed; leaving one out never causes "? more".
	Optional bool
	Rank     int
}

// RunProgress is the live progress of one running apply, keyed by run ID in
// Ctx.Running.
type RunProgress struct {
	Done, Total int
	State       string
	// Errors holds the last error of each step that reported one, by step
	// key. A failed rebuild reports Status done with an Error.
	Errors map[string]string
}

// RunIndex is the runs of the work dir as the root last read them
// (loaders.Runs), off the UI goroutine. Screens read runs here, never from
// Runs directly.
type RunIndex struct {
	// Loaded is false until the first read lands.
	Loaded bool
	Runs   []apply.RunInfo // newest first
	// Warnings are the fixed texts of the runs the read skipped (corrupt),
	// Corrupt their ids.
	Warnings []string
	Corrupt  []string
}

// RunRecovery tells how to release the changes a corrupt run holds: move
// its directory out of the runs directory and restart (the next start
// releases them), keeping it, since it may record a database password
// change. Home shown as ~. Sanitized.
func RunRecovery(workDir, runID string) string {
	runs := shortHome(filepath.Join(workDir, "runs"))
	return "move " + text.Sanitize(filepath.Join(runs, runID)) + " out of " + text.Sanitize(runs) + " (keep it: it may record a database password change), then restart"
}

// shortHome shows a path under the home directory as ~/….
func shortHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
			return filepath.Join("~", rest)
		}
	}
	return p
}

// Ctx is the shared context every screen receives. The root model owns it and
// keeps it current.
type Ctx struct {
	API      *railway.Client
	Store    *store.Store
	Views    *view.Views
	CS       *changeset.ChangeSet
	Reg      apply.Registry
	Runs     *apply.Runs
	Settings *settings.Settings
	Theme    Theme
	Icons    Icons
	Account  string
	// Token is the token source and expiry as text, e.g. "config · expires 14:05".
	Token string
	// NerdDetected is the startup font scan result; Icons "auto" follows it.
	NerdDetected bool
	Width        int
	Height       int
	IsTTY        bool
	Now          func() time.Time
	Running      map[string]RunProgress
	// RunIndex is the runs index; the root refreshes it after every run
	// event (coalesced) and on RefreshRunsMsg.
	RunIndex RunIndex
	// Filters holds each list screen's filter text by key, for the session:
	// the root owns the map and every screen shares it.
	Filters map[string]string
	// Open builds a screen by name; nil when the name is unknown.
	Open func(name string, args ...any) Screen
	// Live runs Railway subscriptions; nil when there is no websocket (the
	// screens then poll only).
	Live *railway.Live
	// Send delivers a message to the program from another goroutine (Live
	// callbacks forward through it); nil in tests without a program.
	Send func(tea.Msg)
}

// Screen is one view on the stack. Messages reach only the top screen, so a
// hidden screen's ticks stop by themselves. View receives the body size left
// after the frame chrome.
type Screen interface {
	Init(*Ctx) tea.Cmd
	Update(*Ctx, tea.Msg) (Screen, tea.Cmd)
	View(ctx *Ctx, width, height int) string
	Title() string
	Keys() []KeyHint
}

// Resumer is implemented by screens that must restart work (polling,
// subscriptions) when they become the top screen again.
type Resumer interface {
	Resume(*Ctx) tea.Cmd
}

// Suspender is implemented by screens that must stop work when they are no
// longer on top.
type Suspender interface {
	Suspend(*Ctx)
}

// Capturer is implemented by screens that take raw key input (a text field,
// a prompt): while Capturing reports true the root handles no global key and
// passes every key to the screen.
type Capturer interface {
	Capturing() bool
}

// KeyClaimer is implemented by screens that bind a key the root otherwise
// handles globally (C, ?, q, esc…): when Claims(key) is true the root
// forwards the key to the screen instead. ctrl+c is never claimed, so
// quitting stays safe. key is tea.KeyMsg.String().
type KeyClaimer interface {
	Claims(key string) bool
}

// Noter is implemented by screens with a note for the right of the frame
// header, or a standing footer message shown while no flash is up (Notice's
// zero value shows nothing).
type Noter interface {
	Note(*Ctx) string
	Notice(*Ctx) FlashMsg
}
