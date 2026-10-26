package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/men232/railway-manager/internal/workdir"
)

// StepRecord is the journal entry of one step. It never holds change values.
type StepRecord struct {
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
	PatchID string    `json:"patchId,omitempty"`
	At      time.Time `json:"at"`
	// Data is a step's own non-secret data, e.g. tunnel pids of a database
	// password step.
	Data json.RawMessage `json:"data,omitempty"`
	// Expanded holds the keys of the steps this step added through
	// Result.More, so a resume can rebuild them.
	Expanded []string `json:"expanded,omitempty"`
	// Acted means the step may have acted: set by the step through
	// Env.MarkActed right before a state-changing command, and by the
	// executor whenever the status becomes done. Nothing clears it, so a
	// reopened step that then fails, is blocked or is interrupted still
	// shows it acted in an earlier attempt.
	Acted bool `json:"acted,omitempty"`
}

// Journal is runs/<run id>/journal.json: statuses, errors, patch ids and
// timestamps, no values. Every setter saves atomically before returning.
// Read records through Step and Outcome while a run executes.
type Journal struct {
	RunID   string                `json:"runId"`
	State   string                `json:"state"`
	Created time.Time             `json:"created"`
	Steps   map[string]StepRecord `json:"steps"`
	Changes map[string]Outcome    `json:"changes"`

	mu   sync.Mutex
	path string
}

// OpenJournal reads the journal at path. A missing file gives a new journal
// whose RunID is the name of the directory holding path; it is written on
// the first set.
func OpenJournal(path string) (*Journal, error) {
	j := &Journal{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		j.RunID = filepath.Base(filepath.Dir(path))
		j.Created = time.Now().UTC()
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, j); err != nil {
			return nil, fmt.Errorf("journal %s: %w", path, err)
		}
	}
	if j.Steps == nil {
		j.Steps = map[string]StepRecord{}
	}
	if j.Changes == nil {
		j.Changes = map[string]Outcome{}
	}
	return j, nil
}

// Set records a step's state and saves the journal. Acted is sticky: a
// record that has it keeps it whatever rec says.
func (j *Journal) Set(key string, rec StepRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Steps[key].Acted {
		rec.Acted = true // sticky: no writer clears it (StepRecord.Acted)
	}
	j.Steps[key] = rec
	return j.save()
}

// SetOutcome records what the run did to a change and saves the journal.
func (j *Journal) SetOutcome(changeID string, o Outcome) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Changes[changeID] = o
	return j.save()
}

// SetState records the run state and saves the journal.
func (j *Journal) SetState(state string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.State = state
	return j.save()
}

// Step returns a step's record (zero when none).
func (j *Journal) Step(key string) StepRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Steps[key]
}

// Outcome returns a change's outcome (zero when none).
func (j *Journal) Outcome(changeID string) Outcome {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Changes[changeID]
}

// snapshot copies the step records.
func (j *Journal) snapshot() map[string]StepRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return maps.Clone(j.Steps)
}

func (j *Journal) outcomes() map[string]Outcome {
	j.mu.Lock()
	defer j.mu.Unlock()
	return maps.Clone(j.Changes)
}

func (j *Journal) save() error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(j.path, data, 0o600)
}
