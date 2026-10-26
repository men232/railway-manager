package dbops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/proc"
)

// sleeps counts the verify loop's waits and never blocks.
type sleeps struct {
	n   int
	gap []time.Duration
	err error
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.n++
	s.gap = append(s.gap, d)
	return s.err
}

func memberIs(i int) func(proc.Cmd) bool {
	return func(c proc.Cmd) bool {
		uri, _ := envOf(c, "RM_MONGO_URI")
		return strings.Contains(uri, encodeURIComponent(mongoTun[i].Socket))
	}
}

func TestVerifyMongo(t *testing.T) {
	instantRetry(t)
	t.Run("primary", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("mongosh", memberIs(0), proc.FakeReply{Stdout: "WAITING 5\n", Err: &proc.ExitError{Code: 4}})
		f.On("mongosh", memberIs(1), proc.FakeReply{Stdout: "PRIMARY\n"})
		var s sleeps
		res, err := VerifyMongo(context.Background(), f, "root", mongoTun, newPw, s.sleep)
		if err != nil || res != (Result{Outcome: Primary, Member: 1}) || res.String() != "primary (member 2)" || s.n != 0 {
			t.Fatalf("res=%+v err=%v sleeps=%d", res, err, s.n)
		}
		c := f.Calls()[1]
		if !slices.Equal(c.Args, []string{"--nodb", "--norc", "--quiet", "--eval", mongoVerifyScript}) {
			t.Errorf("argv %q", c.Args)
		}
		if pw, _ := envOf(c, "RM_MONGO_PASSWORD"); pw != newPw {
			t.Error("password not in env")
		}
		if uri, _ := envOf(c, "RM_MONGO_URI"); !strings.HasPrefix(uri, "mongodb://root@") {
			t.Errorf("uri %q", uri)
		}
		noSecretInArgv(t, f)
	})
	t.Run("waiting then primary", func(t *testing.T) {
		f := &proc.Fake{}
		n := 0
		f.On("mongosh", func(proc.Cmd) bool { n++; return n < 3 }, proc.FakeReply{Stdout: "WAITING 0\n", Err: &proc.ExitError{Code: 4}})
		f.On("mongosh", nil, proc.FakeReply{Stdout: "PRIMARY\n"})
		var s sleeps
		res, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], newPw, s.sleep)
		if err != nil || res.Outcome != Primary || s.n != 2 || s.gap[0] != 5*time.Second {
			t.Fatalf("res=%+v err=%v sleeps=%d", res, err, s.n)
		}
	})
	t.Run("repaired", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("mongosh", nil, proc.FakeReply{Stdout: "REPAIRED\n"})
		var s sleeps
		res, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], newPw, s.sleep)
		if err != nil || res.Outcome != Repaired || res.String() != "repaired: replica config re-applied, primary again (member 1)" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	for _, lost := range []string{"LOST_MULTI", "STILL_LOST MongoServerError: x"} {
		t.Run(lost, func(t *testing.T) {
			f := &proc.Fake{}
			f.On("mongosh", nil, proc.FakeReply{Stdout: lost + "\n", Err: &proc.ExitError{Code: 5}})
			var s sleeps
			_, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], newPw, s.sleep)
			if err == nil || !strings.Contains(err.Error(), "member 1") || !strings.Contains(err.Error(), "cfg.version") ||
				!strings.Contains(err.Error(), "force: true") || s.n != 0 {
				t.Fatalf("err=%v sleeps=%d", err, s.n)
			}
		})
	}
	t.Run("not primary after 180s", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("mongosh", memberIs(0), proc.FakeReply{Stdout: "WAITING 0\n", Err: &proc.ExitError{Code: 4}})
		f.On("mongosh", nil, exitErr("MongoServerSelectionError: connection timed out "+newPw))
		var s sleeps
		_, err := VerifyMongo(context.Background(), f, "root", mongoTun[:2], newPw, s.sleep)
		noSecretInErr(t, err)
		if !strings.HasPrefix(err.Error(), "not primary after 180s: member 2: mongosh: MongoServerSelectionError") || s.n != 35 {
			t.Fatalf("err=%v sleeps=%d", err, s.n)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("mongosh", nil, proc.FakeReply{Stdout: "WAITING 0\n", Err: &proc.ExitError{Code: 4}})
		s := sleeps{err: context.Canceled}
		_, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], newPw, s.sleep)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("incomplete output", func(t *testing.T) {
		f := &proc.Fake{}
		f.On("mongosh", nil, proc.FakeReply{Stdout: "PRI", Err: errorsWaitDelay()})
		var s sleeps
		_, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], newPw, s.sleep)
		if !errors.Is(err, ErrIncomplete) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no members", func(t *testing.T) {
		if _, err := VerifyMongo(context.Background(), &proc.Fake{}, "root", nil, newPw, (&sleeps{}).sleep); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestVerifyMongoNoConfig(t *testing.T) {
	instantRetry(t)
	f := &proc.Fake{}
	f.On("mongosh", nil, proc.FakeReply{Stdout: "NO_CONFIG\n", Err: &proc.ExitError{Code: 5}})
	_, err := VerifyMongo(context.Background(), f, "root", mongoTun[:1], newPw, (&sleeps{}).sleep)
	if err == nil || !strings.Contains(err.Error(), "member 1 has no replica set config") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(mongoVerifyScript, "print('NO_CONFIG')") {
		t.Error("script does not tell NO_CONFIG apart")
	}
}

// The 180 s bound is wall clock: once spent, no member is tried again.
func TestVerifyMongoStopsAtDeadline(t *testing.T) {
	instantRetry(t)
	now := time.Unix(1000, 0)
	old := clock
	t.Cleanup(func() { clock = old })
	clock = func() time.Time { return now }
	f := &proc.Fake{}
	f.On("mongosh", func(proc.Cmd) bool { now = now.Add(100 * time.Second); return true }, proc.FakeReply{Stdout: "WAITING 0\n", Err: &proc.ExitError{Code: 4}})
	_, err := VerifyMongo(context.Background(), f, "root", mongoTun, newPw, (&sleeps{}).sleep)
	if err == nil || !strings.HasPrefix(err.Error(), "not primary after 180s") || len(f.Calls()) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(f.Calls()))
	}
	if got := verifyRunTimeout(now.Add(-170 * time.Second)); got != 10*time.Second {
		t.Errorf("run timeout %v", got)
	}
	if got := verifyRunTimeout(now); got != verifyTimeout {
		t.Errorf("run timeout %v", got)
	}
}
