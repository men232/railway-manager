// Package changeset holds the user's local list of uncommitted Railway
// changes, its persistence, and the conflict check between change sets.
package changeset

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/men232/railway-manager/internal/domain"
)

// Base is what the target was when the change was made. Value is set only for
// non-secret targets; secrets keep a fingerprint.
type Base struct {
	Absent      bool   `json:"absent,omitempty"`
	Sealed      bool   `json:"sealed,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Value       string `json:"value,omitempty"`
}

// Origin records who made a change, e.g. a rotation row and its group letter.
type Origin struct {
	Producer string `json:"producer"`
	Row      string `json:"row,omitempty"`
	Letter   string `json:"letter,omitempty"`
	// Source is how the producer got the value, for screens that say so:
	// SourceGenerate, or "" when not recorded.
	Source string `json:"source,omitempty"`
}

// SourceGenerate is the Origin.Source of a value the producer generated.
const SourceGenerate = "generate"

// PasswordGroupPrefix starts the group of every change of one database
// password ("rotate-db:<database key>:<user>", rotation.DBGroupKey).
const PasswordGroupPrefix = "rotate-db:"

// LetterRewrite is the Origin.Letter of a change a database password
// decision made by itself (the password change, the own variable, a URL
// rewritten with the new password): it is rebuilt when the password is
// decided again. Any other change at a place of the group is the user's
// decision and is kept.
const LetterRewrite = "rewrite"

// Change is one uncommitted change, resolved when made and self-contained after.
type Change struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Place    domain.Place    `json:"place"`
	Target   string          `json:"target"`
	New      json.RawMessage `json:"new,omitempty"`
	Base     Base            `json:"base"`
	Origin   *Origin         `json:"origin,omitempty"`
	Group    string          `json:"group,omitempty"`
	Requires []string        `json:"requires,omitempty"`
	// RunID is set in the live set while a run holds the change.
	RunID string `json:"runId,omitempty"`
	// Flag is the last run's reason for keeping the change.
	Flag    string    `json:"flag,omitempty"`
	Created time.Time `json:"created"`
}

// Key identifies the target of the change: place and target name.
func (c Change) Key() string {
	return c.Place.Key() + "|" + c.Target
}

// NewID returns 16 random hex characters.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("changeset: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
