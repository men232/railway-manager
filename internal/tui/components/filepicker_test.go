package components

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/tui"
)

type fakeEntry struct {
	name string
	dir  bool
}

func (f fakeEntry) Name() string { return f.name }
func (f fakeEntry) IsDir() bool  { return f.dir }
func (f fakeEntry) Type() fs.FileMode {
	if f.dir {
		return fs.ModeDir
	}
	return 0
}
func (f fakeEntry) Info() (fs.FileInfo, error) { return fakeInfo{f}, nil }

type fakeInfo struct{ fakeEntry }

func (fakeInfo) Size() int64         { return 0 }
func (f fakeInfo) Mode() fs.FileMode { return f.Type() }
func (fakeInfo) ModTime() time.Time  { return time.Time{} }
func (fakeInfo) Sys() any            { return nil }

// fakeDirs maps a folder path to its entry names; a trailing "/" marks a folder.
type fakeDirs map[string][]string

func (d fakeDirs) ReadDir(dir string) ([]fs.DirEntry, error) {
	names, ok := d[dir]
	if !ok {
		return nil, errors.New("no such folder")
	}
	var out []fs.DirEntry
	for _, n := range names {
		out = append(out, fakeEntry{strings.TrimSuffix(n, "/"), strings.HasSuffix(n, "/")})
	}
	return out, nil
}

func newPicker(d fakeDirs) *FilePicker {
	p := NewFilePicker(d, "/home/u")
	p.cwd = "/work"
	return p
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func names(p *FilePicker) string {
	ms, _ := p.matches()
	var s []string
	for _, m := range ms {
		if m.dir {
			s = append(s, m.name+"/")
		} else {
			s = append(s, m.name)
		}
	}
	return strings.Join(s, ",")
}

func TestFilePickerPrefixThenSubstringCaseInsensitive(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {"my.key", "KEYS", "keyring", "a_key", "other"}})
	p.Update(typ("KEY"))
	if got := names(p); got != "KEYS,keyring,a_key,my.key" {
		t.Errorf("matches = %q", got)
	}
}

func TestFilePickerDotfilesOnlyWithDot(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {".env", ".git/", "a"}})
	if got := names(p); got != "a" {
		t.Errorf("no dot: %q", got)
	}
	p.Update(typ("."))
	if got := names(p); got != ".env,.git/" {
		t.Errorf("dot: %q", got)
	}
}

func TestFilePickerFoldersEndInSlashAndEnterOpens(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {"sub/", "f"}, "/work/sub": {"inner.txt"}})
	if got := names(p); got != "f,sub/" {
		t.Fatalf("matches = %q", got)
	}
	p.Update(key(tea.KeyDown))
	if msg := run(p.Update(key(tea.KeyEnter))); msg != nil {
		t.Fatalf("opening a folder emitted %v", msg)
	}
	if got := string(p.ed.buf); got != "sub/" {
		t.Errorf("typed = %q", got)
	}
	if got := names(p); got != "inner.txt" {
		t.Errorf("inside = %q", got)
	}
	got, ok := run(p.Update(key(tea.KeyEnter))).(FilePickedMsg)
	if !ok || got.Path != "/work/sub/inner.txt" {
		t.Errorf("picked = %+v", got)
	}
}

func TestFilePickerCycleWraps(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {"a", "b", "c"}})
	p.Update(key(tea.KeyUp))
	if p.list.Cursor != 2 {
		t.Errorf("up wraps to %d", p.list.Cursor)
	}
	p.Update(key(tea.KeyDown))
	if p.list.Cursor != 0 {
		t.Errorf("down wraps to %d", p.list.Cursor)
	}
}

func TestFilePickerTabCompletes(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {"secret.txt", "sub/"}})
	p.Update(typ("se"))
	if msg := run(p.Update(key(tea.KeyTab))); msg != nil {
		t.Fatalf("tab emitted %v", msg)
	}
	if got := string(p.ed.buf); got != "secret.txt" {
		t.Errorf("file: %q", got)
	}
	p.Update(key(tea.KeyCtrlU))
	p.Update(typ("su"))
	p.Update(key(tea.KeyTab))
	if got := string(p.ed.buf); got != "sub/" {
		t.Errorf("folder: %q", got)
	}
}

func TestFilePickerBackspaceCtrlUEsc(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {"abc"}})
	p.Update(typ("abc"))
	p.Update(key(tea.KeyBackspace))
	if got := string(p.ed.buf); got != "ab" {
		t.Errorf("backspace: %q", got)
	}
	p.Update(key(tea.KeyCtrlU))
	if got := string(p.ed.buf); got != "" {
		t.Errorf("ctrl+u: %q", got)
	}
	if _, ok := run(p.Update(key(tea.KeyEsc))).(CancelMsg); !ok {
		t.Error("esc should cancel")
	}
}

func TestFilePickerTildeExpands(t *testing.T) {
	p := newPicker(fakeDirs{"/home/u": {"k.pem", "d/"}, "/home/u/d": {"x"}})
	p.Update(typ("~"))
	p.Update(key(tea.KeyEnter))
	if got := string(p.ed.buf); got != "~/" {
		t.Fatalf("typed = %q", got)
	}
	if got := names(p); got != "d/,k.pem" {
		t.Errorf("home = %q", got)
	}
	p.Update(typ("d/"))
	got, ok := run(p.Update(key(tea.KeyEnter))).(FilePickedMsg)
	if !ok || got.Path != "/home/u/d/x" {
		t.Errorf("picked = %+v", got)
	}
}

func TestFilePickerAbsoluteAndRelative(t *testing.T) {
	p := newPicker(fakeDirs{"/etc": {"hosts"}, "/work/rel": {"r"}})
	p.Update(typ("/etc/"))
	if got := names(p); got != "hosts" {
		t.Errorf("abs = %q", got)
	}
	p.Update(key(tea.KeyCtrlU))
	p.Update(typ("rel/"))
	if got := names(p); got != "r" {
		t.Errorf("rel = %q", got)
	}
}

func TestFilePickerUnreadableAndControlNames(t *testing.T) {
	p := newPicker(fakeDirs{"/work": {"ok", "bad\x1b[31mname"}})
	if got := names(p); got != "ok" {
		t.Errorf("matches = %q", got)
	}
	p.Update(typ("nope/"))
	if v := p.View(60); !strings.Contains(v, "cannot read") {
		t.Errorf("view = %q", v)
	}
	if msg := run(p.Update(key(tea.KeyEnter))); msg != nil {
		t.Errorf("enter with no matches emitted %v", msg)
	}
}

func TestFilePickerViewTenLinesCutNeverWrapped(t *testing.T) {
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, "very-long-file-name-number-"+strings.Repeat("x", 30)+string(rune('a'+i)))
	}
	p := newPicker(fakeDirs{"/work": many})
	p.Theme = tui.NewTheme(termenv.ANSI256)
	for _, w := range []int{80, 60, 20, 5, 1} {
		ls := strings.Split(p.View(w), "\n")
		if len(ls) != 10 {
			t.Fatalf("width %d: %d lines", w, len(ls))
		}
		for _, l := range ls {
			if ansi.StringWidth(l) > w {
				t.Errorf("width %d: line too wide: %q", w, l)
			}
		}
	}
	e := newPicker(fakeDirs{"/work": nil})
	if n := strings.Count(e.View(80), "\n") + 1; n != 10 {
		t.Errorf("empty: %d lines", n)
	}
}

func TestFilePickerScrollsToCursor(t *testing.T) {
	var many []string
	for i := 0; i < 12; i++ {
		many = append(many, string(rune('a'+i)))
	}
	p := newPicker(fakeDirs{"/work": many})
	for range 10 {
		p.Update(key(tea.KeyDown))
	}
	if v := p.View(80); !strings.Contains(v, "› k") {
		t.Errorf("cursor row not visible:\n%s", v)
	}
}

type countingDirs struct {
	fakeDirs
	n int
}

func (c *countingDirs) ReadDir(dir string) ([]fs.DirEntry, error) {
	c.n++
	return c.fakeDirs.ReadDir(dir)
}

func TestFilePickerCachesListingPerFolder(t *testing.T) {
	c := &countingDirs{fakeDirs: fakeDirs{"/work": {"a", "b", "sub/"}, "/work/sub": {"x"}}}
	p := newPicker(nil)
	p.r = c
	p.View(80)
	p.Update(key(tea.KeyDown))
	p.Update(key(tea.KeyUp))
	p.Update(typ("a"))
	p.View(80)
	if c.n != 1 {
		t.Fatalf("ReadDir called %d times in one folder", c.n)
	}
	p.Update(key(tea.KeyCtrlU))
	p.Update(typ("sub/"))
	p.View(80)
	if c.n != 2 {
		t.Errorf("new folder should read once more, calls = %d", c.n)
	}
}

func TestOSDirReaderSymlinkedFolder(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(root, "real"), 0o755))
	must(os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600))
	must(os.Symlink("real", filepath.Join(root, "linkdir")))
	must(os.Symlink("file", filepath.Join(root, "linkfile")))
	must(os.Symlink("nowhere", filepath.Join(root, "dangling")))
	must(os.WriteFile(filepath.Join(root, "real", "in.txt"), []byte("x"), 0o600))
	p := NewFilePicker(OSDirReader{}, root)
	p.cwd = root
	if got := names(p); got != "dangling,file,linkdir/,linkfile,real/" {
		t.Fatalf("matches = %q", got)
	}
	p.Update(typ("linkd"))
	p.Update(key(tea.KeyEnter))
	if got := string(p.ed.buf); got != "linkdir/" {
		t.Fatalf("typed = %q", got)
	}
	got, ok := run(p.Update(key(tea.KeyEnter))).(FilePickedMsg)
	if !ok || got.Path != filepath.Join(root, "linkdir", "in.txt") {
		t.Errorf("picked = %+v", got)
	}
	p.Update(key(tea.KeyCtrlU))
	p.Update(typ("linkf"))
	if got, ok := run(p.Update(key(tea.KeyEnter))).(FilePickedMsg); !ok || got.Path != filepath.Join(root, "linkfile") {
		t.Errorf("symlinked file picked = %+v", got)
	}
}

func TestFilePickerHelpKeepsEscAtWidth60(t *testing.T) {
	p := newPicker(fakeDirs{"/work": nil})
	ls := strings.Split(p.View(60), "\n")
	if !strings.Contains(ls[9], "esc") {
		t.Errorf("help = %q", ls[9])
	}
}
