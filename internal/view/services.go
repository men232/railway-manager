package view

import (
	"strings"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// Views bundles the selectors and their caches. Like the store it is used
// only from the Update goroutine.
type Views struct {
	Store *store.Store
	Reg   apply.Registry
	// Compile compiles a change set into a step graph; injected at wiring so
	// this package does not import the kinds.
	Compile func(changeset.ChangeSet) (apply.Graph, error)
	// Me is the signed-in Railway user's id ("" until known): a patch is
	// taken for a local run by its message only when Me made it.
	Me string

	services Cache[[]ServiceRow]
	vars     Cache[[]VarRow]
	csView   Cache[ChangeSetView]
	monitor  Cache[[]MonitorRow]
	detail   Cache[Detail]
	tree     Cache[[]GroupRow]
	feed     Cache[[]FeedRow]
	rotate   Cache[[]RotateRow]
	stalled  Cache[[]StalledRun]

	monitorBuilds int // Monitor's row builds, for tests
	treeBuilds    int // Tree's builds, for tests
	feedBuilds    int // Feed's builds, for tests
	rotateBuilds  int // Rotate's builds, for tests
	stalledBuilds int // Stalled's builds, for tests

	// The runs index part of Rotate's memo key, for the index whose first
	// run and length these are (runsKey).
	runsKeyFirst *apply.RunInfo
	runsKeyLen   int
	runsKeyIndex string
}

// ServiceRow is one service of one environment with its pending changes.
type ServiceRow struct {
	Place                 domain.Place
	Project, Env, Service string
	Pending               int
}

// Services lists the services of the environments with the number of
// change-set changes at each.
func (v *Views) Services(cs *changeset.ChangeSet, envIDs []string) []ServiceRow {
	k := key(append(rev(v.Store, store.Catalog), csRev(cs), strings.Join(envIDs, ","))...)
	return v.services.Get(k, func() []ServiceRow {
		pending := map[string]int{}
		for _, c := range changes(cs) {
			pending[c.Place.Key()]++
		}
		insts := v.Store.Instances(envIDs)
		rows := make([]ServiceRow, len(insts))
		for i, in := range insts {
			rows[i] = ServiceRow{
				Place: in.Place, Project: in.Project, Env: in.Env, Service: in.Service,
				Pending: pending[in.Place.Key()],
			}
		}
		return rows
	})
}
