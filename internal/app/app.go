// Package app is the entry point: the cobra root command, its flags, exit
// codes and the wiring of every package into the running program. It holds
// no business logic.
package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/men232/railway-manager/internal/railway"
	appversion "github.com/men232/railway-manager/internal/version"
)

// Exit codes.
const (
	ExitOK           = 0
	ExitError        = 1
	ExitInvalidInput = 2
	ExitAuth         = 3
)

var version, commit, buildDate = "", "", ""

// SetVersion sets what --version prints.
func SetVersion(v, c, d string) { version, commit, buildDate = v, c, d }

// Main runs the CLI with the build version and exits with its code. Both
// entry points (main.go and cmd/railway-manager) call it.
func Main() {
	SetVersion(appversion.Version, appversion.Commit, appversion.BuildDate)
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, startTUI))
}

// usageError is a bad flag or argument.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func newRootCmd(start func(Options) error) *cobra.Command {
	var o Options
	cmd := &cobra.Command{
		Use:           "railway-manager",
		Short:         "Manage Railway projects from a terminal UI",
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version, commit, buildDate),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.NoArgs(cmd, args); err != nil {
				return &usageError{err}
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error { return start(o) },
	}
	cmd.SetVersionTemplate("railway-manager {{.Version}}\n")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	f := cmd.Flags()
	f.StringSliceVar(&o.Projects, "project", nil, "projects to show this run, by name or id (comma-separated); overrides the saved selection")
	f.StringSliceVar(&o.Envs, "env", nil, "environments to show this run, by name or id (comma-separated); overrides the saved selection")
	return cmd
}

// run executes the command line and returns the exit code.
func run(args []string, stdout, stderr io.Writer, start func(Options) error) int {
	cmd := newRootCmd(start)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.Execute()
	if err == nil {
		return ExitOK
	}
	fmt.Fprintln(stderr, "railway-manager:", err)
	var ue *usageError
	var ae *railway.AuthError
	switch {
	case errors.As(err, &ue):
		return ExitInvalidInput
	case errors.As(err, &ae):
		return ExitAuth
	default:
		return ExitError
	}
}

// startTUI builds the program against the real environment and runs it.
func startTUI(o Options) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	o.Home, o.Getenv, o.Endpoint = home, os.Getenv, railway.DefaultEndpoint
	o.LiveEndpoint = railway.LiveEndpoint
	return runTUI(o)
}

// runTUI builds the program from o and runs it; the live socket is closed
// before it returns. SIGTERM and SIGHUP (the terminal closed) quit it as a
// quit command does, so the process leaves through the same path.
func runTUI(o Options) error {
	m, program, err := build(o)
	if err != nil {
		return err
	}
	defer quitOn(program, syscall.SIGTERM, syscall.SIGHUP)()
	return runProgram(program, m.Ctx().Live)
}

// quitOn quits p when one of sigs arrives, until the returned stop is called.
// A signal before p runs is delivered once it does.
func quitOn(p *tea.Program, sigs ...os.Signal) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			p.Quit()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
