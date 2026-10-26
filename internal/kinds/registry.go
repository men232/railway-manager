// Package kinds holds the change kinds and the steps they run: variables
// (set, delete), limits and volume sizes through the environment patch,
// redeploys, restarts and domains as their own steps, database passwords
// and KEYFILEs through SSH tunnels, and the keep marker.
package kinds

import (
	"github.com/men232/railway-manager/internal/apply"
)

// NewRegistry returns every kind of this package by name.
func NewRegistry() apply.Registry {
	reg := apply.Registry{}
	for _, k := range []apply.Kind{
		variableKind{del: false},
		variableKind{del: true},
		redeployKind{},
		restartKind{},
		limitsKind{},
		volumeKind{},
		domainKind{name: kindServiceDomainAdd},
		domainKind{name: kindServiceDomainDel},
		domainKind{name: kindCustomDomainAdd},
		domainKind{name: kindCustomDomainDel},
		dbPasswordKind{},
		dbKeyFileKind{},
		keepKind{},
	} {
		reg[k.Name()] = k
	}
	return reg
}

// StopRunError is an error that stops the whole run for a human to look at:
// a target changed in Railway after the run acted on it. The
// executor recognises it through StopRun without importing this package.
type StopRunError struct{ Msg string }

func (e *StopRunError) Error() string { return e.Msg }

// StopRun reports that the run must stop.
func (e *StopRunError) StopRun() bool { return true }
