package apply

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/store"
)

// ErrCycle means the steps' dependencies form a cycle.
var ErrCycle = errors.New("dependency cycle")

const (
	redeployPrefix = "redeploy:"
	restartPrefix  = "restart:"
	// DBGroupPrefix starts the group of every change of one database
	// password: its redeploys wire per place and wait for SUCCESS.
	DBGroupPrefix = "rotate-db:"
)

// Graph is a compiled run: its steps by key and a deterministic
// topological order.
type Graph struct {
	Steps map[string]Step
	Order []string
	// Reopen maps a step key to the keys that reopen it: a step whose
	// journal record is done or skipped runs again, acting, while any listed
	// step in the graph is not done or skipped.
	Reopen map[string][]string
}

// wired is a step whose dependencies the compiler extended or pruned.
type wired struct {
	Step
	deps []string
}

func (w wired) Deps() []string { return w.deps }
func (w wired) Unwrap() Step   { return w.Step }

// action is a redeploy or restart at one place.
type action struct {
	key      string
	pk       string       // place key, from the step key
	place    domain.Place // for newRedeployStep
	serves   map[string]bool
	declared []string // Deps() of explicit redeploy and collapsed restart steps
	own      Step     // the kind's restart step, nil for a redeploy
}

// Compile turns a frozen change set into a DAG:
//
//   - one patch step per env from the kinds' Patch fragments (newPatchStep);
//   - the kinds' OwnSteps; redeploy:<place> and restart:<place> own steps are
//     actions, the rest are write steps like the patches;
//   - redeploys derived from Redeploys minus cs.RedeployOff, merged with an
//     explicit redeploy at the same place (newRedeployStep); a restart at a
//     place that also redeploys collapses into the redeploy, which serves both;
//   - a redeploy or restart at a place depends on the write steps of every
//     change at that place or redeploying it, and on the write steps of every
//     change sharing a Group with those changes, in any env; a write step that
//     reaches the action through declared deps (a verify after its redeploy)
//     is left out instead;
//   - declared Deps() of an explicit redeploy step or of a restart that
//     collapses into a redeploy are kept on the redeploy, and a declared dep
//     on such a restart means the redeploy;
//   - a group starting with "rotate-db:" (a database password) is not wired
//     across places: an action depends only on the write steps of changes at
//     or redeploying its place, and a redeploy
//     serving a change of such a group waits for SUCCESS (wait true);
//   - a step serving c depends on the write steps serving each change in
//     c.Requires (or, when such a change has none, the actions serving it);
//   - a dependency on a step absent from the graph is dropped;
//   - Reopen lists, under each write step serving a change of a Reopener
//     kind and opting in through Reopenable, the write steps serving the
//     changes that require that change.
func Compile(cs changeset.ChangeSet, s *store.Store, reg Registry,
	newPatchStep func(envID string, changes []changeset.Change) Step,
	newRedeployStep func(p domain.Place, serves []string, deps []string, wait bool) Step,
) (Graph, error) {
	byID := make(map[string]changeset.Change, len(cs.Changes))
	index := make(map[string]int, len(cs.Changes))
	for i, c := range cs.Changes {
		if _, ok := reg[c.Kind]; !ok {
			return Graph{}, fmt.Errorf("change %s: unknown kind %s", c.ID, c.Kind)
		}
		byID[c.ID], index[c.ID] = c, i
	}
	for _, c := range cs.Changes {
		for _, r := range c.Requires {
			if _, ok := byID[r]; !ok {
				return Graph{}, fmt.Errorf("change %s requires a change outside this run: %s", c.ID, r)
			}
		}
	}

	writes := map[string]Step{}
	addWrite := func(st Step) error {
		if _, dup := writes[st.Key()]; dup {
			return fmt.Errorf("duplicate step %s", st.Key())
		}
		writes[st.Key()] = st
		return nil
	}

	// Patch steps, one per env, envs in order of first change.
	var envs []string
	byEnv := map[string][]changeset.Change{}
	for _, c := range cs.Changes {
		if _, ok := reg[c.Kind].Patch(c); ok {
			if _, seen := byEnv[c.Place.EnvID]; !seen {
				envs = append(envs, c.Place.EnvID)
			}
			byEnv[c.Place.EnvID] = append(byEnv[c.Place.EnvID], c)
		}
	}
	for _, e := range envs {
		if err := addWrite(newPatchStep(e, byEnv[e])); err != nil {
			return Graph{}, err
		}
	}

	// Own steps; redeploys and restarts become actions.
	actions := map[string]*action{}
	actionAt := func(key string, p domain.Place) *action {
		a := actions[key]
		if a == nil {
			a = &action{key: key, pk: p.Key(), place: p, serves: map[string]bool{}}
			actions[key] = a
		}
		return a
	}
	restarts := map[string]Step{}
	for _, c := range cs.Changes {
		for _, st := range reg[c.Kind].OwnSteps(c) {
			switch k := st.Key(); {
			case strings.HasPrefix(k, redeployPrefix):
				if k != redeployPrefix+c.Place.Key() {
					return Graph{}, fmt.Errorf("step %s is not at the place of change %s", k, c.ID)
				}
				a := actionAt(k, c.Place)
				a.declared = append(a.declared, st.Deps()...)
				for _, id := range st.Serves() {
					a.serves[id] = true
				}
			case strings.HasPrefix(k, restartPrefix):
				if _, dup := restarts[k]; dup {
					return Graph{}, fmt.Errorf("duplicate step %s", k)
				}
				restarts[k] = st
			default:
				if err := addWrite(st); err != nil {
					return Graph{}, err
				}
			}
		}
	}

	// Derived redeploys.
	redeploysOf := map[string][]domain.Place{}
	for _, c := range cs.Changes {
		ps := reg[c.Kind].Redeploys(s, c)
		redeploysOf[c.ID] = ps
		for _, p := range ps {
			if slices.Contains(cs.RedeployOff, p.Key()) {
				continue
			}
			actionAt(redeployPrefix+p.Key(), p).serves[c.ID] = true
		}
	}

	// Restarts collapse into a redeploy at the same place; a dependency on
	// the collapsed restart becomes one on the redeploy.
	collapsed := map[string]string{}
	resolve := func(d string) string {
		if r, ok := collapsed[d]; ok {
			return r
		}
		return d
	}
	for k, st := range restarts {
		pk := strings.TrimPrefix(k, restartPrefix)
		if a := actions[redeployPrefix+pk]; a != nil {
			collapsed[k] = a.key
			a.declared = append(a.declared, st.Deps()...)
			for _, id := range st.Serves() {
				a.serves[id] = true
			}
			continue
		}
		a := &action{key: k, pk: pk, own: st, declared: st.Deps(), serves: map[string]bool{}}
		for _, id := range st.Serves() {
			a.serves[id] = true
		}
		actions[k] = a
	}

	for k := range actions {
		if _, dup := writes[k]; dup {
			return Graph{}, fmt.Errorf("duplicate step %s", k)
		}
	}

	writeBy := servedBy(writes)
	actionBy := map[string][]string{}
	for k, a := range actions {
		for id := range a.serves {
			actionBy[id] = append(actionBy[id], k)
		}
	}
	requiredBy := func(c changeset.Change) []string {
		var out []string
		for _, r := range c.Requires {
			if ws := writeBy[r]; len(ws) > 0 {
				out = append(out, ws...)
			} else {
				out = append(out, actionBy[r]...)
			}
		}
		return out
	}

	// reaches reports whether from reaches to through declared deps.
	declaredOf := func(k string) []string {
		if st, ok := writes[k]; ok {
			return st.Deps()
		}
		if a, ok := actions[k]; ok {
			return a.declared
		}
		return nil
	}
	reaches := func(from, to string) bool {
		seen := map[string]bool{from: true}
		stack := []string{from}
		for len(stack) > 0 {
			k := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, d := range declaredOf(k) {
				d = resolve(d)
				if d == to {
					return true
				}
				if !seen[d] {
					seen[d] = true
					stack = append(stack, d)
				}
			}
		}
		return false
	}
	writesBefore := func(k, changeID string) []string {
		var out []string
		for _, w := range writeBy[changeID] {
			if !reaches(w, k) {
				out = append(out, w)
			}
		}
		return out
	}

	extra := map[string][]string{}
	for k, st := range writes {
		for _, id := range st.Serves() {
			if c, ok := byID[id]; ok {
				extra[k] = append(extra[k], requiredBy(c)...)
			}
		}
	}
	for k, a := range actions {
		groups := map[string]bool{}
		for _, c := range cs.Changes {
			at := c.Place.Key() == a.pk || slices.ContainsFunc(redeploysOf[c.ID], func(p domain.Place) bool { return p.Key() == a.pk })
			if at {
				extra[k] = append(extra[k], writesBefore(k, c.ID)...)
				if c.Group != "" && !strings.HasPrefix(c.Group, DBGroupPrefix) {
					groups[c.Group] = true
				}
			}
		}
		for _, c := range cs.Changes {
			if groups[c.Group] {
				extra[k] = append(extra[k], writesBefore(k, c.ID)...)
			}
		}
		for id := range a.serves {
			extra[k] = append(extra[k], requiredBy(byID[id])...)
		}
	}

	present := func(k string) bool {
		_, w := writes[k]
		_, a := actions[k]
		return w || a
	}
	finalDeps := func(k string, declared []string) []string {
		var out []string
		for _, d := range slices.Concat(declared, extra[k]) {
			d = resolve(d)
			if d != k && present(d) {
				out = append(out, d)
			}
		}
		slices.Sort(out)
		return slices.Compact(out)
	}

	g := Graph{Steps: make(map[string]Step, len(writes)+len(actions))}
	keep := func(k string, st Step) {
		deps := finalDeps(k, st.Deps())
		declared := slices.Clone(st.Deps())
		slices.Sort(declared)
		if slices.Equal(slices.Compact(declared), deps) {
			g.Steps[k] = st
		} else {
			g.Steps[k] = wired{Step: st, deps: deps}
		}
	}
	for k, st := range writes {
		keep(k, st)
	}
	for k, a := range actions {
		if a.own != nil {
			keep(k, a.own)
			continue
		}
		serves := make([]string, 0, len(a.serves))
		wait := false
		for id := range a.serves {
			serves = append(serves, id)
			wait = wait || strings.HasPrefix(byID[id].Group, DBGroupPrefix)
		}
		slices.SortFunc(serves, func(x, y string) int { return index[x] - index[y] })
		keep(k, newRedeployStep(a.place, serves, finalDeps(k, a.declared), wait))
	}

	for _, c := range cs.Changes {
		if r, ok := reg[c.Kind].(Reopener); !ok || !r.Reopen() {
			continue
		}
		var by []string
		for _, d := range cs.Changes {
			if slices.Contains(d.Requires, c.ID) {
				by = append(by, writeBy[d.ID]...)
			}
		}
		for _, k := range writeBy[c.ID] {
			if !reopenable(writes[k]) {
				continue
			}
			list := slices.DeleteFunc(slices.Concat(g.Reopen[k], by), func(x string) bool { return x == k })
			if len(list) == 0 {
				continue
			}
			slices.Sort(list)
			if g.Reopen == nil {
				g.Reopen = map[string][]string{}
			}
			g.Reopen[k] = slices.Compact(list)
		}
	}

	order, err := topoOrder(g.Steps)
	if err != nil {
		return Graph{}, err
	}
	g.Order = order
	return g, nil
}

// servedBy maps a change id to the keys of the steps serving it.
func servedBy(steps map[string]Step) map[string][]string {
	out := map[string][]string{}
	for k, st := range steps {
		for _, id := range st.Serves() {
			out[id] = append(out[id], k)
		}
	}
	return out
}

// topoOrder is Kahn's algorithm, level by level, keys sorted in each level.
func topoOrder(steps map[string]Step) ([]string, error) {
	indeg := make(map[string]int, len(steps))
	dependents := map[string][]string{}
	for k, st := range steps {
		indeg[k] += 0
		for _, d := range st.Deps() {
			indeg[k]++
			dependents[d] = append(dependents[d], k)
		}
	}
	var level []string
	for k, n := range indeg {
		if n == 0 {
			level = append(level, k)
		}
	}
	order := make([]string, 0, len(steps))
	for len(level) > 0 {
		slices.Sort(level)
		order = append(order, level...)
		var next []string
		for _, k := range level {
			for _, d := range dependents[k] {
				if indeg[d]--; indeg[d] == 0 {
					next = append(next, d)
				}
			}
		}
		level = next
	}
	if len(order) < len(steps) {
		var stuck []string
		for k, n := range indeg {
			if n > 0 {
				stuck = append(stuck, k)
			}
		}
		slices.Sort(stuck)
		return nil, fmt.Errorf("%w: %s", ErrCycle, strings.Join(stuck, ", "))
	}
	return order, nil
}
