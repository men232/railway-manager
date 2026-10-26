package dbops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/men232/railway-manager/internal/proc"
)

const sshConfig = `# BEGIN railway:p:e:s
# Railway service: mongo
Host railway-mongo-single-replica
    HostName ssh.railway.com
    User 00000000-0000-4000-8000-000000000001
    ServerAliveInterval 30
# END railway:p:e:s
`

func TestSSHTarget(t *testing.T) {
	f := &proc.Fake{}
	f.On("railway", nil, proc.FakeReply{Stdout: sshConfig})
	got, err := SSHTarget(context.Background(), f, "p", "e", "s")
	if err != nil || got != "00000000-0000-4000-8000-000000000001@ssh.railway.com" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	calls := f.Calls()
	want := []string{"ssh", "config", "--dry-run", "-p", "p", "-e", "e", "-s", "s"}
	if len(calls) != 1 || calls[0].Name != "railway" || !slices.Equal(calls[0].Args, want) {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestSSHTargetNoAccess(t *testing.T) {
	for name, reply := range map[string]proc.FakeReply{
		"no user":     {Stdout: "Host x\n    HostName ssh.railway.com\n"},
		"no hostname": {Stdout: "Host x\n    User u\n"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &proc.Fake{}
			f.On("railway", nil, reply)
			_, err := SSHTarget(context.Background(), f, "p", "e", "svc-1")
			if err == nil || !strings.Contains(err.Error(), "no SSH access to svc-1") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSSHTargetCLIError(t *testing.T) {
	f := &proc.Fake{}
	f.On("railway", nil, proc.FakeReply{Err: &proc.ExitError{Code: 1, LastStderr: "Unauthorized\x1b[0m"}})
	_, err := SSHTarget(context.Background(), f, "p", "e", "svc-1")
	if err == nil || err.Error() != "railway ssh config: exit status 1: Unauthorized" {
		t.Fatalf("err=%v", err)
	}
}

func TestSSHTargetContextError(t *testing.T) {
	f := &proc.Fake{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := SSHTarget(ctx, f, "p", "e", "svc-1")
	if !errors.Is(err, context.Canceled) || err.Error() != "railway ssh config: context canceled" {
		t.Fatalf("err=%v", err)
	}
}

func TestSSHTargetRejectsOptionLikeValues(t *testing.T) {
	for name, out := range map[string]string{
		"user":     "Host x\n    HostName ssh.railway.com\n    User -oProxyCommand=evil\n",
		"hostname": "Host x\n    HostName -oProxyCommand=evil\n    User u\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := &proc.Fake{}
			f.On("railway", nil, proc.FakeReply{Stdout: out})
			_, err := SSHTarget(context.Background(), f, "p", "e", "svc-1")
			if err == nil || err.Error() != "unexpected ssh config for svc-1" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
