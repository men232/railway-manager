package dbops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
)

// mongoVerifyScript checks a restarted member, reads
// preferring secondaries: PRIMARY when it is the writable primary; WAITING
// <state> while replSetGetStatus waits for another reason; when the member
// is lost from its own config (code 93, "not a member", "valid entry"),
// NO_CONFIG when it has no stored config, LOST_MULTI for a set of several
// members, else a forced reconfig of the same config one version up, then
// REPAIRED once primary within 30 s, or STILL_LOST.
const mongoVerifyScript = mongoConnect + `
conn.setReadPref('secondaryPreferred');
const admin = conn.getDB('admin');
const primary = () => { try { return admin.runCommand({ hello: 1 }).isWritablePrimary; } catch (e) { return false; } };
if (primary()) { print('PRIMARY'); quit(0); }
let st;
try { st = admin.runCommand({ replSetGetStatus: 1 }); } catch (e) { st = { ok: 0, code: e.code, errmsg: e.message }; }
if (!(st.code === 93 || /not a member|valid entry/i.test(st.errmsg || ''))) { print('WAITING ' + (st.myState ?? st.errmsg ?? '')); quit(4); }
const cfg = conn.getDB('local').getCollection('system.replset').findOne();
if (!cfg) { print('NO_CONFIG'); quit(5); }
if (cfg.members.length !== 1) { print('LOST_MULTI'); quit(5); }
cfg.version += 1;
try { admin.runCommand({ replSetReconfig: cfg, force: true }); } catch (e) { print('STILL_LOST ' + e.message); quit(6); }
for (let i = 0; i < 30; i++) { if (primary()) { print('REPAIRED'); quit(0); } sleep(1000); }
print('STILL_LOST'); quit(6);
`

// Outcome is how a Mongo verify ended well.
type Outcome int

const (
	// Primary: a member is the writable primary.
	Primary Outcome = iota + 1
	// Repaired: a lost single member was given its config back by force.
	Repaired
)

// Result is a verify that ended well. Member indexes the members given.
type Result struct {
	Outcome Outcome
	Member  int
}

func (r Result) String() string {
	if r.Outcome == Repaired {
		return fmt.Sprintf("repaired: replica config re-applied, primary again (member %d)", r.Member+1)
	}
	return fmt.Sprintf("primary (member %d)", r.Member+1)
}

const (
	verifyFor   = 180 * time.Second
	verifyGap   = 5 * time.Second
	verifyRound = int(verifyFor / verifyGap) // rounds at most, whatever the clock says
)

// clock is the verify loop's time; tests replace it.
var clock = time.Now

// verifyRunTimeout bounds one verify run by 90 s and by what is left of the
// 180 s since start.
func verifyRunTimeout(start time.Time) time.Duration {
	return min(verifyTimeout, verifyFor-clock().Sub(start))
}

// handRepair is the failure for a member lost from its own replica set config
// that was not repaired automatically.
func handRepair(member int, multi bool) error {
	why := "the forced reconfig did not make it primary"
	if multi {
		why = "the set has several members, and forcing a config onto one could split it"
	}
	return fmt.Errorf("member %d is lost from its own replica set config and was not repaired (%s); repair by hand in mongosh on that member: "+
		"cfg = db.getSiblingDB('local').system.replset.findOne(); cfg.version++; rs.reconfig(cfg, {force: true})", member+1, why)
}

// VerifyMongo checks that a restarted Mongo has a writable primary again,
// logging in to each member as user with password: up to
// 180 s, rounds over the members 5 s apart (sleep waits; Sleep is the real
// one). A single member lost from its own config is repaired by a forced
// reconfig; a lost member of a larger set, or a failed repair, fails with the
// hand-repair steps. The error never holds the password.
func VerifyMongo(ctx context.Context, r proc.Runner, user string, members []Tunnel, password string,
	sleep func(context.Context, time.Duration) error) (res Result, err error) {
	defer func() { err = scrub(err, password) }()
	if len(members) == 0 {
		return Result{}, errors.New("no tunnel")
	}
	if user == "" || password == "" {
		return Result{}, errNoCreds
	}
	start := clock()
	last := "no answer"
	for round := 1; ; round++ {
		for i, m := range members {
			timeout := verifyRunTimeout(start)
			if timeout <= 0 {
				break
			}
			cmd, err := mongoCmd(mongoVerifyScript, m, user, "admin", password)
			if err != nil {
				return Result{}, err
			}
			out, _, err := runOnce(ctx, r, cmd, timeout, nil, []string{password})
			if err != nil && stop(ctx, err) {
				return Result{}, err
			}
			word, rest, _ := strings.Cut(lastLine(out), " ")
			switch word {
			case "PRIMARY":
				if err == nil {
					return Result{Outcome: Primary, Member: i}, nil
				}
			case "REPAIRED":
				if err == nil {
					return Result{Outcome: Repaired, Member: i}, nil
				}
			case "LOST_MULTI":
				return Result{}, handRepair(i, true)
			case "STILL_LOST":
				return Result{}, handRepair(i, false)
			case "NO_CONFIG":
				return Result{}, fmt.Errorf("member %d has no replica set config to re-apply (local.system.replset is empty); "+
					"it needs a manual repair, such as rs.initiate() on that member", i+1)
			case "WAITING":
				last = fmt.Sprintf("member %d: waiting %s", i+1, short(rest, password))
				continue
			}
			if err == nil {
				err = errors.New("mongosh: unexpected output")
			}
			last = fmt.Sprintf("member %d: %s", i+1, err)
		}
		if round >= verifyRound || clock().Sub(start) >= verifyFor {
			return Result{}, fmt.Errorf("not primary after 180s: %s", last)
		}
		if err := sleep(ctx, verifyGap); err != nil {
			return Result{}, err
		}
	}
}

// short is untrusted text cut to 100 characters, redacted.
func short(s, secret string) string {
	s = text.Sanitize(redact(strings.TrimSpace(s), []string{secret}))
	if rs := []rune(s); len(rs) > 100 {
		return string(rs[:100]) + "…"
	}
	return s
}
