package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/root"
	"github.com/men232/railway-manager/internal/tui/screens/picker"
)

// testOptions points the wiring at the fake server and a temp home, with an
// env token, so nothing reaches the network or the real home directory.
func testOptions(t *testing.T) (Options, *railwaytest.Fake) {
	t.Helper()
	f := railwaytest.New(t)
	f.SetCatalog(railway.Catalog{Projects: []railway.Project{{
		ID: "p1", Name: "acme", Workspace: "ws",
		Environments: []railway.Environment{{ID: "e1", Name: "dev", CanAccess: true, ServiceIDs: []string{"s1"}}},
		Services:     []railway.Service{{ID: "s1", Name: "api"}},
	}}})
	env := map[string]string{"RAILWAY_TOKEN": "test-token"}
	return Options{
		Home:         t.TempDir(),
		Getenv:       func(k string) string { return env[k] },
		Endpoint:     f.URL,
		LiveEndpoint: f.WSURL,
		FontDirs:     []string{},
		Program:      []tea.ProgramOption{tea.WithInput(nil), tea.WithOutput(&bytes.Buffer{})},
	}, f
}

// runAll executes cmd and every command a batch holds, returning the
// messages.
func runAll(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if b, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, b...)
			continue
		}
		out = append(out, msg)
	}
	return out
}

func TestWiringBuildsApp(t *testing.T) {
	o, _ := testOptions(t)
	m, p, err := build(o)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m == nil || p == nil {
		t.Fatalf("build returned model %v, program %v", m, p)
	}
	work := filepath.Join(o.Home, ".railway-manager")
	fi, err := os.Stat(work)
	if err != nil {
		t.Fatalf("work dir: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("work dir mode = %o, want 700", fi.Mode().Perm())
	}
	if got := m.Ctx().Settings.Icons; got != "auto" {
		t.Errorf("Settings.Icons = %q, want the default auto", got)
	}
	if got := m.Ctx().Token; got != "env:RAILWAY_TOKEN" {
		t.Errorf("Ctx.Token = %q, want env:RAILWAY_TOKEN", got)
	}
	var acct *loaders.AccountMsg
	for _, msg := range runAll(m.Init()) {
		if a, ok := msg.(loaders.AccountMsg); ok {
			acct = &a
		}
	}
	if acct == nil {
		t.Fatalf("Init did not produce the WhoAmI command")
	}
	if acct.Err != nil || acct.Account.Email != "fake@example.test" {
		t.Errorf("WhoAmI = %+v, %v", acct.Account, acct.Err)
	}
}

func TestCorruptChangeSetNote(t *testing.T) {
	o, _ := testOptions(t)
	work := filepath.Join(o.Home, ".railway-manager")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "changeset.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := wire(o)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if !strings.HasPrefix(d.LoadNote, "change set was unreadable, moved to ") {
		t.Errorf("LoadNote = %q", d.LoadNote)
	}
	if len(d.CS.Changes) != 0 {
		t.Errorf("change set not empty after a corrupt file")
	}
}

// A change set file that parses but holds changes their kinds refuse (or of
// a kind this build does not know) loads whole: the invalid changes are
// kept, marked and named in the startup note, judged before anything is read
// from Railway (the store is still empty).
func TestInvalidChangesLoadedMarked(t *testing.T) {
	o, _ := testOptions(t)
	work := filepath.Join(o.Home, ".railway-manager")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	file := `{"changes":[
	 {"id":"ok","kind":"variable.set","place":{"projectId":"p1","envId":"e1","serviceId":"s1"},"target":"A","new":{"value":"x"},"base":{"absent":true},"created":"2026-10-24T12:00:00Z"},
	 {"id":"bad-limits","kind":"limits.set","place":{"projectId":"p1","envId":"e1","serviceId":"s1"},"target":"limits","new":{"cpu":2},"base":{},"created":"2026-10-24T12:00:00Z"},
	 {"id":"bad-kind","kind":"service.teleport","place":{"projectId":"p1","envId":"e1","serviceId":"s1"},"target":"teleport","base":{},"created":"2026-10-24T12:00:00Z"}
	]}`
	if err := os.WriteFile(filepath.Join(work, "changeset.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := wire(o)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if d.LoadNote != "2 changes in the change set are invalid and will not be applied · c change set" {
		t.Errorf("LoadNote = %q", d.LoadNote)
	}
	if len(d.CS.Changes) != 3 {
		t.Fatalf("changes = %+v, want all three kept", d.CS.Changes)
	}
	marks := map[string]string{}
	for _, g := range d.Views.ChangeSet(d.CS, nil).Groups {
		for _, r := range g.Changes {
			marks[r.Change.ID] = r.Invalid
		}
	}
	if marks["ok"] != "" || marks["bad-limits"] == "" || marks["bad-kind"] != "unknown change kind service.teleport" {
		t.Errorf("marks = %q", marks)
	}
}

func TestWorkDirFromEnv(t *testing.T) {
	o, _ := testOptions(t)
	custom := filepath.Join(t.TempDir(), "rm-home")
	get := o.Getenv
	o.Getenv = func(k string) string {
		if k == "RAILWAY_MANAGER_HOME" {
			return custom
		}
		return get(k)
	}
	d, err := wire(o)
	if err != nil {
		t.Fatal(err)
	}
	if d.SettingsPath != filepath.Join(custom, "settings.json") || d.Runs.Dir != custom {
		t.Errorf("paths = %s, runs %s; want under %s", d.SettingsPath, d.Runs.Dir, custom)
	}
}

// openPatterns find the screen names opened in source: OpenMsg{Name: "x"},
// Open("x") / open("x"), and Home's menu entries {"key", "Label", "x", dim}.
var openPatterns = []*regexp.Regexp{
	regexp.MustCompile(`OpenMsg\{Name:\s*"([^"]+)"`),
	regexp.MustCompile(`\b[oO]pen\("([^"]+)"`),
	regexp.MustCompile(`\{"[^"]*",\s*"[^"]+",\s*"([^"]+)",\s*"[^"]*",\s*(?:true|false),`), // Home's menu
}

// openedNames scans the non-test Go files under dirs.
func openedNames(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range openPatterns {
				for _, m := range re.FindAllSubmatch(src, -1) {
					found[string(m[1])] = path
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

// Every name a screen or the root opens must resolve.
func TestScreensResolve(t *testing.T) {
	o, _ := testOptions(t)
	m, _, err := build(o)
	if err != nil {
		t.Fatal(err)
	}
	done := func([]string) tea.Msg { return nil }
	// Arguments for the screens that need some; others are opened bare.
	args := map[string][]any{
		"keys":          {"Home", []tui.KeyHint{{Key: "s", Label: "services"}}},
		"run":           {"20261024-abc"},
		"variables":     {domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}},
		"domains":       {domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}},
		"logs":          {domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}},
		"picker":        {"Select projects", []picker.Item{{ID: "p1", Label: "acme"}}, map[string]bool{}, done},
		"rotate":        {"fingerprint"},
		"rotate.places": {"rowkey"},
	}
	names := openedNames(t, filepath.Join("..", "tui", "screens"), filepath.Join("..", "tui", "root"))
	for _, want := range []string{"services", "changeset", "settings", "keys", "run", "variables", "history", "picker", "domains", "logs", "rotate", "rotate.places"} {
		if _, ok := names[want]; !ok {
			t.Errorf("scan missed %q: the patterns no longer match the source", want)
		}
	}
	for name, file := range names {
		if s := m.Ctx().Open(name, args[name]...); s == nil {
			t.Errorf("Open(%q) (opened in %s) = nil", name, file)
		}
	}
	for name := range screens() {
		if _, ok := names[name]; !ok {
			t.Errorf("screen %q registered but never opened", name)
		}
	}
}

func TestTokenInfoNeverRefreshes(t *testing.T) {
	o, _ := testOptions(t)
	o.Getenv = func(string) string { return "" }
	cfgDir := filepath.Join(o.Home, ".railway")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Expired, so a Get would refresh.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"user":{"token":"tok","tokenExpiresAt":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o.Refresh = func(context.Context) error {
		t.Error("TokenInfo ran a token refresh")
		return nil
	}
	d, err := wire(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.TokenInfo(); !strings.HasPrefix(got, "config · expires ") || strings.Contains(got, "tok\"") {
		t.Errorf("TokenInfo = %q", got)
	}
}

func TestRunsReadSnapshotAndWriteEnvs(t *testing.T) {
	o, _ := testOptions(t)
	d, err := wire(o)
	if err != nil {
		t.Fatal(err)
	}
	d.Settings.WriteEnvs = []string{"e1"}
	root.New(d) // publishes the snapshot and the write envs
	snap := d.Snapshot.Load()
	if snap == nil || snap == d.Store {
		t.Fatalf("snapshot = %p, live store %p: want a separate copy", snap, d.Store)
	}
	env := d.Runs.NewEnv("r1", nil)
	if s, ok := env.Store.(*store.Store); !ok || s != snap {
		t.Errorf("Env.Store = %v, want the snapshot", env.Store)
	}
	if env.Sleep == nil || env.Now == nil || env.API == nil || env.Proc == nil {
		t.Errorf("Env not fully wired: %+v", env)
	}
	if d.Runs.Exec.Concurrency != 6 {
		t.Errorf("Concurrency = %d, want 6", d.Runs.Exec.Concurrency)
	}
	if !d.Client.WriteAllowed("e1") || d.Client.WriteAllowed("e2") {
		t.Errorf("WriteAllowed does not follow the published write envs")
	}
	d.Settings.WriteEnvs = nil // an unsaved edit must not change the guard
	if !d.Client.WriteAllowed("e1") {
		t.Errorf("WriteAllowed read the Settings struct, not WriteEnvs")
	}
}

func TestSelectionFlags(t *testing.T) {
	o, _ := testOptions(t)
	o.Projects = []string{" acme", "", "p9 "}
	o.Envs = []string{"dev"}
	d, err := wire(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.SelectProjects, ",") != "acme,p9" || strings.Join(d.SelectEnvs, ",") != "dev" {
		t.Errorf("selection = %q / %q", d.SelectProjects, d.SelectEnvs)
	}
}

func TestNerdFontScan(t *testing.T) {
	with := t.TempDir()
	nested := filepath.Join(with, "truetype", "jetbrains")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "JetBrainsMonoNerdFont-Regular.ttf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	without := t.TempDir()
	if err := os.WriteFile(filepath.Join(without, "Menlo.ttc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(without, "nope")
	if !nerdFontInstalled([]string{missing, without, with}) {
		t.Errorf("nested Nerd font not found")
	}
	if nerdFontInstalled([]string{missing, without}) {
		t.Errorf("found a Nerd font where there is none")
	}

	for _, tc := range []struct {
		icons string
		dirs  []string
		want  bool
	}{
		{"auto", []string{with}, true},
		{"auto", []string{without}, false},
		{"plain", []string{with}, false},
		{"nerd", []string{without}, true},
	} {
		o, _ := testOptions(t)
		o.FontDirs = tc.dirs
		work := filepath.Join(o.Home, ".railway-manager")
		if err := os.MkdirAll(work, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "settings.json"), []byte(`{"icons":"`+tc.icons+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := wire(o)
		if err != nil {
			t.Fatal(err)
		}
		if d.Icons.Nerd != tc.want || d.NerdDetected != (tc.dirs[0] == with) {
			t.Errorf("icons %s, dirs %v: Nerd = %v, detected %v", tc.icons, tc.dirs, d.Icons.Nerd, d.NerdDetected)
		}
	}
}

func TestFontDirs(t *testing.T) {
	mac := fontDirs("darwin", "/h")
	if strings.Join(mac, ",") != "/h/Library/Fonts,/Library/Fonts,/System/Library/Fonts" {
		t.Errorf("darwin = %v", mac)
	}
	linux := fontDirs("linux", "/h")
	if strings.Join(linux, ",") != "/h/.local/share/fonts,/h/.fonts,/usr/share/fonts" {
		t.Errorf("linux = %v", linux)
	}
}

func TestTokenInfo(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		st   railway.TokenStatus
		want string
	}{
		{railway.TokenStatus{Token: "secret-a", Source: "env:RAILWAY_TOKEN"}, "env:RAILWAY_TOKEN"},
		{railway.TokenStatus{Token: "secret-b", Source: "config", ExpiresAt: now.Add(2*time.Hour + 5*time.Minute)}, "config · expires 14:05"},
		{railway.TokenStatus{Token: "secret-c", Source: "config", ExpiresAt: now.Add(48 * time.Hour)}, "config · expires 2026-10-26 12:00"},
		{railway.TokenStatus{Token: "secret-d", Source: "config", ExpiresAt: now.Add(-time.Minute), RefreshErr: "railway whoami: exit 1"}, "config · expires 11:59 · refresh failed"},
		{railway.TokenStatus{Missing: "no ~/.railway/config.json: run railway login"}, "none: no ~/.railway/config.json: run railway login"},
	}
	for _, tc := range cases {
		got := tokenInfo(tc.st, now)
		if got != tc.want {
			t.Errorf("tokenInfo(%s) = %q, want %q", tc.st.Source, got, tc.want)
		}
		if tc.st.Token != "" && strings.Contains(got, tc.st.Token) {
			t.Errorf("token text leaks the token")
		}
	}
}

func TestVersionFlag(t *testing.T) {
	SetVersion("v1.2.3", "abc1234", "2026-10-24")
	t.Cleanup(func() { SetVersion("", "", "") })
	var out, errOut bytes.Buffer
	started := false
	code := run([]string{"--version"}, &out, &errOut, func(Options) error { started = true; return nil })
	if code != ExitOK || started {
		t.Fatalf("code = %d, started = %v", code, started)
	}
	if got := out.String(); got != "railway-manager v1.2.3 (commit abc1234, built 2026-10-24)\n" {
		t.Errorf("--version printed %q", got)
	}
}

func TestRunFlagsAndExitCodes(t *testing.T) {
	var got Options
	start := func(o Options) error { got = o; return nil }
	var out, errOut bytes.Buffer
	if code := run([]string{"--project", "acme,p2", "--env", "dev"}, &out, &errOut, start); code != ExitOK {
		t.Fatalf("code = %d, stderr %s", code, errOut.String())
	}
	if strings.Join(got.Projects, ",") != "acme,p2" || strings.Join(got.Envs, ",") != "dev" {
		t.Errorf("options = %+v", got)
	}

	for _, args := range [][]string{{"--nope"}, {"extra"}} {
		errOut.Reset()
		if code := run(args, &out, &errOut, start); code != ExitInvalidInput {
			t.Errorf("%v: code = %d, want %d", args, code, ExitInvalidInput)
		}
		if errOut.Len() == 0 {
			t.Errorf("%v: no error printed", args)
		}
	}

	cases := []struct {
		err  error
		want int
	}{
		{errors.New("boom"), ExitError},
		{&railway.AuthError{Msg: "no token"}, ExitAuth},
	}
	for _, tc := range cases {
		errOut.Reset()
		code := run(nil, &out, &errOut, func(Options) error { return tc.err })
		if code != tc.want {
			t.Errorf("%v: code = %d, want %d", tc.err, code, tc.want)
		}
		if !strings.Contains(errOut.String(), tc.err.Error()) {
			t.Errorf("%v: stderr = %q", tc.err, errOut.String())
		}
	}
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// liveGoroutines counts the goroutines running Live's code.
func liveGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "railway.(*Live)") {
			n++
		}
	}
	return n
}

// The app builds one Live on the HTTP client's token source, keeps it idle
// until something subscribes, and closes the socket on every way out.
func TestLiveWiredAndClosedOnQuit(t *testing.T) {
	quits := []struct {
		name    string
		quit    func(*tea.Program)
		wantErr bool
	}{
		{"q", func(p *tea.Program) { p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}) }, false},
		{"ctrl+c", func(p *tea.Program) { p.Send(tea.KeyMsg{Type: tea.KeyCtrlC}) }, false},
		{"program error", func(p *tea.Program) { p.Kill() }, true},
	}
	for _, tc := range quits {
		t.Run(tc.name, func(t *testing.T) {
			o, f := testOptions(t)
			m, p, err := build(o)
			if err != nil {
				t.Fatal(err)
			}
			live := m.Ctx().Live
			if live == nil {
				t.Fatal("Ctx.Live is nil")
			}
			done := make(chan error, 1)
			go func() { done <- runProgram(p, live) }()

			// Idle until a screen subscribes: Home does not.
			time.Sleep(50 * time.Millisecond)
			if n := len(f.WSRequests()); n != 0 || live.State() != "idle" {
				t.Fatalf("before any subscription: %d dials, state %s", n, live.State())
			}

			live.Subscribe("subscription { x }", nil, func(json.RawMessage) {}, func(string, railway.LiveFail) {})
			eventually(t, "the socket to open", func() bool { return live.State() == "open" && f.WSOpen() == 1 })
			if inits := f.WSInits(); len(inits) != 1 || inits[0]["Authorization"] != "Bearer test-token" {
				t.Errorf("connection_init payloads = %d, want one with the env token", len(inits))
			}
			for _, r := range f.WSRequests() {
				if strings.Contains(r.URI, "test-token") {
					t.Errorf("token in the websocket URL")
				}
			}

			// A key pops a screen (the startup picker may be up) and quits
			// from the last one: repeat until the program ends.
			var runErr error
		quit:
			for i := 0; ; i++ {
				tc.quit(p)
				select {
				case runErr = <-done:
					break quit
				case <-time.After(100 * time.Millisecond):
				}
				if i == 50 {
					t.Fatal("program did not end")
				}
			}
			if (runErr != nil) != tc.wantErr {
				t.Errorf("run error = %v, want error %v", runErr, tc.wantErr)
			}
			if live.State() != "idle" {
				t.Errorf("state after quit = %s, want idle", live.State())
			}
			if n := liveGoroutines(); n != 0 {
				t.Errorf("%d Live goroutines still running after quit", n)
			}
			eventually(t, "the server to see the socket close", func() bool { return f.WSOpen() == 0 })
		})
	}
}

// SIGHUP and SIGTERM quit the program as a quit command does: Run returns
// without error and Live is closed.
func TestSignalQuitsProgram(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			o, _ := testOptions(t)
			o.Program = append(o.Program, tea.WithoutSignalHandler())
			m, p, err := build(o)
			if err != nil {
				t.Fatal(err)
			}
			stop := quitOn(p, sig)
			defer stop()
			live := m.Ctx().Live
			done := make(chan error, 1)
			go func() { done <- runProgram(p, live) }()
			time.Sleep(50 * time.Millisecond)
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("run error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("program did not end")
			}
			if live.State() != "idle" {
				t.Errorf("state after quit = %s, want idle", live.State())
			}
		})
	}
}

// Quitting through the cobra command returns cleanly with Live closed.
func TestRunTUIQuitsThroughCommand(t *testing.T) {
	o, _ := testOptions(t)
	// One q per read (a burst would arrive as one "qqq" key) until the
	// command returns: the startup picker may take the first.
	in, keys := io.Pipe()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				_, _ = keys.Write([]byte("q"))
			}
		}
	}()
	t.Cleanup(func() { close(stop); keys.Close() })
	o.Program = []tea.ProgramOption{tea.WithInput(in), tea.WithOutput(&bytes.Buffer{})}
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut, func(Options) error { return runTUI(o) }); code != ExitOK {
		t.Fatalf("code = %d, stderr %q", code, errOut.String())
	}
	if n := liveGoroutines(); n != 0 {
		t.Errorf("%d Live goroutines after the command returned", n)
	}
}

// hintKeys is the keys of hs joined by spaces, for a containment check.
func hintKeys(hs []tui.KeyHint) string {
	var ks []string
	for _, h := range hs {
		ks = append(ks, h.Key)
	}
	return " " + strings.Join(ks, " ") + " "
}

// "variables" bare is the tree (with grouping and unfolding); with a place,
// which Services enter passes, it is the scoped view of that place.
func TestVariablesRegistryTreeAndScoped(t *testing.T) {
	o, _ := testOptions(t)
	m, _, err := build(o)
	if err != nil {
		t.Fatal(err)
	}
	tree := m.Ctx().Open("variables")
	if tree == nil || tree.Title() != "Variables" {
		t.Fatalf("Open(variables) = %v", tree)
	}
	if k := hintKeys(tree.Keys()); !strings.Contains(k, " g ") || !strings.Contains(k, " enter ") {
		t.Errorf("tree keys = %q, want g and enter", k)
	}
	scoped := m.Ctx().Open("variables", domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"})
	if scoped == nil || scoped.Title() != "Variables" {
		t.Fatalf("Open(variables, place) = %v", scoped)
	}
	if k := hintKeys(scoped.Keys()); strings.Contains(k, " g ") {
		t.Errorf("scoped keys = %q, want no grouping", k)
	}
	if h := m.Ctx().Open("history"); h == nil || h.Title() != "History" {
		t.Errorf("Open(history) = %v", h)
	}
	if h := m.Ctx().Open("history", "DATABASE_URL"); h == nil {
		t.Errorf("Open(history, name) = nil")
	}
}

// The key list (?) over the tree and over History lists their keys.
func TestKeysListTreeAndHistory(t *testing.T) {
	o, _ := testOptions(t)
	m, _, err := build(o)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	for _, c := range []struct {
		name string
		want []string
	}{
		{"variables", []string{"unfold", "by value", "reveal", "history"}},
		{"history", []string{"open run", "more", "refresh"}},
	} {
		m.Update(tui.OpenMsg{Name: c.name})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
		out := m.View()
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s key list misses %q:\n%s", c.name, w, out)
			}
		}
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	}
}
