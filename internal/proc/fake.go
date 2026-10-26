package proc

import (
	"context"
	"fmt"
	"sync"
)

// FakeReply is what a scripted command answers.
type FakeReply struct {
	Stdout, Stderr string
	Err            error
	Pid            int // pid reported by Start; 0 means a generated one
}

type fakeRule struct {
	name  string
	match func(Cmd) bool
	reply FakeReply
}

// Fake is a scripted Runner, safe for concurrent use. It records every Run and
// Start, Env and Stdin included, so tests can scan them for leaked secrets.
type Fake struct {
	mu       sync.Mutex
	rules    []fakeRule
	calls    []Cmd
	paths    map[string]string
	cmdlines map[int]string
	nextPid  int
	procs    []*FakeProcess
}

var _ Runner = (*Fake)(nil)

// On scripts a reply for commands named name for which match (nil: any) is
// true. The first matching rule wins; match must not call the Fake (its lock is held); an unscripted command fails.
func (f *Fake) On(name string, match func(Cmd) bool, reply FakeReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, fakeRule{name, match, reply})
}

// SetPath makes LookPath(name) succeed with path; others fail.
func (f *Fake) SetPath(name, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paths == nil {
		f.paths = map[string]string{}
	}
	f.paths[name] = path
}

// SetCmdline scripts Cmdline(pid).
func (f *Fake) SetCmdline(pid int, line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cmdlines == nil {
		f.cmdlines = map[int]string{}
	}
	f.cmdlines[pid] = line
}

// Calls returns a copy of every recorded Cmd, in order.
func (f *Fake) Calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Cmd, len(f.calls))
	for i, c := range f.calls {
		out[i] = cloneCmd(c)
	}
	return out
}

// Processes returns the processes started so far.
func (f *Fake) Processes() []*FakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*FakeProcess(nil), f.procs...)
}

func cloneCmd(c Cmd) Cmd {
	c.Args = append([]string(nil), c.Args...)
	c.Env = append([]string(nil), c.Env...)
	c.Unset = append([]string(nil), c.Unset...)
	return c
}

func (f *Fake) record(c Cmd) (FakeReply, error) {
	f.calls = append(f.calls, cloneCmd(c))
	for _, r := range f.rules {
		if r.name == c.Name && (r.match == nil || r.match(cloneCmd(c))) {
			return r.reply, nil
		}
	}
	return FakeReply{}, fmt.Errorf("proc: no scripted reply for %s", c.Name)
}

func (f *Fake) Run(ctx context.Context, c Cmd) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	f.mu.Lock()
	r, err := f.record(c)
	f.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	return r.Stdout, r.Stderr, r.Err
}

func (f *Fake) Start(ctx context.Context, c Cmd) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.record(c)
	if err != nil {
		return nil, err
	}
	if r.Err != nil {
		return nil, r.Err
	}
	pid := r.Pid
	if pid == 0 {
		f.nextPid++
		pid = 40000 + f.nextPid
	}
	p := &FakeProcess{pid: pid, done: make(chan struct{})}
	f.procs = append(f.procs, p)
	return p, nil
}

func (f *Fake) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.paths[name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

func (f *Fake) Cmdline(pid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.cmdlines[pid]; ok {
		return l, nil
	}
	return "", fmt.Errorf("no process %d", pid)
}

// FakeProcess runs until Stop or Exit.
type FakeProcess struct {
	pid     int
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	stopped bool
	err     error
}

func (p *FakeProcess) Pid() int { return p.pid }

func (p *FakeProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *FakeProcess) Stop() error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}

// Exit ends the process on its own with err, as if it died.
func (p *FakeProcess) Exit(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	})
}

// Stopped reports whether Stop was called.
func (p *FakeProcess) Stopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}
