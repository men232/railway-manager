package components

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
)

// DirReader lists a folder. Production code passes OSDirReader; tests a fake.
type DirReader interface {
	ReadDir(dir string) ([]fs.DirEntry, error)
}

// OSDirReader reads real folders with os.ReadDir.
type OSDirReader struct{}

// ReadDir lists dir. A symlink to a folder reports IsDir true; a dangling or
// file symlink stays a file.
func (OSDirReader) ReadDir(dir string) ([]fs.DirEntry, error) {
	es, err := os.ReadDir(dir)
	for i, e := range es {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		if st, serr := os.Stat(filepath.Join(dir, e.Name())); serr == nil && st.IsDir() {
			es[i] = linkedDir{e}
		}
	}
	return es, err
}

// linkedDir is a symlink entry whose target is a folder.
type linkedDir struct{ fs.DirEntry }

func (linkedDir) IsDir() bool { return true }

// FilePickedMsg carries the chosen file's path. Reading the file is the
// caller's job.
type FilePickedMsg struct{ Path string }

const (
	filePickerMatches = 8
	// FilePickerHeight is the number of lines View always returns.
	FilePickerHeight = filePickerMatches + 2
)

// FilePicker is a fixed-height bottom panel that completes a path from the
// typed folder. It never scrolls the terminal: View is
// always FilePickerHeight lines.
type FilePicker struct {
	// Theme colours the folders (blue) and the selected match. The zero value
	// draws plain text.
	Theme tui.Theme

	r    DirReader
	home string
	cwd  string
	ed   editor
	list List

	// listing of the last folder read, reused until the folder part changes
	cacheDir    string
	cacheOK     bool
	cacheFailed bool
	cache       []fileMatch
}

// NewFilePicker returns a picker over r. "~" expands to home. A typed path
// without a leading "/" or "~" is relative to the process working directory
// (read once here; home when it cannot be read).
func NewFilePicker(r DirReader, home string) *FilePicker {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = home
	}
	return &FilePicker{r: r, home: home, cwd: cwd}
}

type fileMatch struct {
	name string
	dir  bool
}

// split divides the typed text into its folder part (verbatim, empty or
// ending in "/") and the name part being matched.
func (p *FilePicker) split() (dirPart, name string) {
	typed := string(p.ed.buf)
	i := strings.LastIndex(typed, "/")
	return typed[:i+1], typed[i+1:]
}

// resolve turns the typed folder part into a real folder path.
func (p *FilePicker) resolve(dirPart string) string {
	switch {
	case dirPart == "":
		return p.cwd
	case dirPart == "~/":
		return p.home
	case strings.HasPrefix(dirPart, "~/"):
		return filepath.Join(p.home, dirPart[2:])
	case strings.HasPrefix(dirPart, "/"):
		return filepath.Clean(dirPart)
	}
	return filepath.Join(p.cwd, dirPart)
}

// matches lists the entries of the typed folder that fit the typed name:
// prefix matches first (an exact name leads), then substring matches, each by
// name. failed reports that the folder could not be read.
func (p *FilePicker) matches() (ms []fileMatch, failed bool) {
	dirPart, name := p.split()
	dir := p.resolve(dirPart)
	if !p.cacheOK || p.cacheDir != dir {
		p.cacheDir, p.cacheOK, p.cache, p.cacheFailed = dir, true, nil, false
		entries, err := p.r.ReadDir(dir)
		if err != nil {
			p.cacheFailed = true
		}
		for _, e := range entries {
			n := e.Name()
			// a name the screen cannot show faithfully cannot be completed
			// either
			if err == nil && text.Sanitize(n) == n {
				p.cache = append(p.cache, fileMatch{name: n, dir: e.IsDir()})
			}
		}
	}
	if p.cacheFailed {
		return nil, true
	}
	lname := strings.ToLower(name)
	showDot := strings.HasPrefix(name, ".")
	var prefix, sub []fileMatch
	for _, m := range p.cache {
		if strings.HasPrefix(m.name, ".") && !showDot {
			continue
		}
		ln := strings.ToLower(m.name)
		switch {
		case strings.HasPrefix(ln, lname):
			prefix = append(prefix, m)
		case strings.Contains(ln, lname):
			sub = append(sub, m)
		}
	}
	order := func(s []fileMatch) {
		sort.SliceStable(s, func(i, j int) bool {
			if ei, ej := s[i].name == name, s[j].name == name; ei != ej {
				return ei
			}
			return s[i].name < s[j].name
		})
	}
	order(prefix)
	order(sub)
	return append(prefix, sub...), false
}

func (p *FilePicker) setTyped(s string) {
	p.ed.buf = []rune(s)
	p.ed.pos = len(p.ed.buf)
	p.list = List{}
}

// Update handles a key: ↑ ↓ cycle, tab completes, enter opens a folder or
// emits FilePickedMsg, esc and ctrl+c emit CancelMsg, other keys edit.
func (p *FilePicker) Update(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		return tui.Emit(CancelMsg{})
	case tea.KeyUp, tea.KeyDown, tea.KeyTab, tea.KeyEnter:
		return p.navigate(k.Type)
	}
	before := string(p.ed.buf)
	p.ed.edit(k, false)
	if string(p.ed.buf) != before {
		p.list = List{}
	}
	return nil
}

func (p *FilePicker) navigate(key tea.KeyType) tea.Cmd {
	if string(p.ed.buf) == "~" && (key == tea.KeyTab || key == tea.KeyEnter) {
		p.setTyped("~/")
		return nil
	}
	ms, _ := p.matches()
	if len(ms) == 0 {
		return nil
	}
	switch key {
	case tea.KeyUp:
		p.list.Cursor = (p.list.Cursor + len(ms) - 1) % len(ms)
		return nil
	case tea.KeyDown:
		p.list.Cursor = (p.list.Cursor + 1) % len(ms)
		return nil
	}
	m := ms[min(p.list.Cursor, len(ms)-1)]
	dirPart, _ := p.split()
	if m.dir {
		p.setTyped(dirPart + m.name + "/")
		return nil
	}
	if key == tea.KeyTab {
		p.setTyped(dirPart + m.name)
		return nil
	}
	return tui.Emit(FilePickedMsg{Path: filepath.Join(p.resolve(dirPart), m.name)})
}

// View draws exactly FilePickerHeight lines, each cut to width: the typed
// path, up to eight matches (folders end in "/" and are blue), the help line.
func (p *FilePicker) View(width int) string {
	lines := make([]string, 0, FilePickerHeight)
	before, after := string(p.ed.buf[:p.ed.pos]), string(p.ed.buf[p.ed.pos:])
	lines = append(lines, "file: "+before+"█"+after)

	ms, failed := p.matches()
	from, to := p.list.Window(len(ms), filePickerMatches)
	for i := from; i < to; i++ {
		m := ms[i]
		name, style := m.name, p.Theme.Muted
		if m.dir {
			name, style = name+"/", p.Theme.Accent2
		}
		if i == p.list.Cursor {
			lines = append(lines, "› "+p.Theme.Selected.Render(name))
		} else {
			lines = append(lines, "  "+style.Render(name))
		}
	}
	if len(ms) == 0 {
		note := "(no matches)"
		if failed {
			note = "(cannot read this folder)"
		}
		lines = append(lines, "  "+p.Theme.Faint.Render(note))
	}
	for len(lines) < 1+filePickerMatches {
		lines = append(lines, "")
	}
	lines = append(lines, p.Theme.Muted.Render("↑↓ pick · tab complete · enter open · ^U clear · esc cancel"))
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}
