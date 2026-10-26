package railway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
)

// discoveryConcurrency is how many projects DiscoverAll reads at a time.
const discoveryConcurrency = 8

// VarRecord is one variable at one location. ServiceID "" is shared.
// UpdatedAt is UTC "2006-01-02T15:04:05" ("" when unknown). A sealed value
// is unreadable and counts as empty.
type VarRecord struct {
	EnvID, ServiceID, Name, Unrendered, Rendered, UpdatedAt string
	Sealed                                                  bool
}

// DiscoveryTarget is one project to read: its environments and the services
// deployed in each.
type DiscoveryTarget struct {
	ProjectID, ProjectName string
	Environments           []Environment
	ServiceNames           map[string]string // for warning labels
}

// SystemVarNames are the Railway system variables discovery keeps: they
// exist only among the rendered variables and identify a service's hosts.
// They never become VarRecords, so no screen shows them and no change
// targets them.
var SystemVarNames = []string{"RAILWAY_PRIVATE_DOMAIN", "RAILWAY_TCP_PROXY_DOMAIN", "RAILWAY_TCP_PROXY_PORT"}

// SystemRecord is one system variable (SystemVarNames) at one location.
type SystemRecord struct {
	EnvID, ServiceID, Name, Value string
}

type DiscoveryResult struct {
	ProjectID string
	Vars      []VarRecord
	// System holds the SystemVarNames read from the rendered variables.
	System   []SystemRecord
	Warnings []string
	// Unread are the places whose read failed: their variables are missing
	// from Vars, not gone. An environment without access or services is
	// only a warning: there was nothing to read.
	Unread []UnreadPlace
}

// UnreadPlace is a place (one service, "" for the shared variables) whose
// read failed, and why, as its warning says it.
type UnreadPlace struct {
	EnvID, ServiceID, Why string
}

type timestampNode struct {
	Name      string `json:"name"`
	ServiceID string `json:"serviceId"`
	UpdatedAt string `json:"updatedAt"`
	IsSealed  bool   `json:"isSealed"`
}

type timestampPage struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	edges[timestampNode]
}

// notFound is Railway's answer for a service not deployed in an
// environment ("ServiceInstance not found"): a short name, then "not
// found", alone. A longer message that merely holds the words is another
// failure and stays a warning.
var notFound = regexp.MustCompile(`(?i)^[a-z][a-z ]{0,40} not found\.?$`)

func targetFromProject(p Project, envs []Environment) DiscoveryTarget {
	names := make(map[string]string, len(p.Services))
	for _, s := range p.Services {
		names[s.ID] = s.Name
	}
	return DiscoveryTarget{ProjectID: p.ID, ProjectName: p.Name, Environments: envs, ServiceNames: names}
}

// DiscoverEnv reads one environment: the same discovery request restricted
// to it. The environment's services come from a fresh lookup of the project
// (one extra small request), so callers need not hold a catalog. Warnings
// of the underlying Discover are dropped.
func (c *Client) DiscoverEnv(ctx context.Context, projectID, envID string) ([]VarRecord, error) {
	var d struct {
		Project *projectNode `json:"project"`
	}
	if err := c.T.Do(ctx, "ProjectEnvironment", projectEnvironmentDoc, map[string]any{"projectId": projectID}, &d); err != nil {
		return nil, err
	}
	if d.Project == nil {
		return nil, fmt.Errorf("railway: project %s not found", projectID)
	}
	p := d.Project.project("")
	for _, e := range p.Environments {
		if e.ID != envID {
			continue
		}
		res, err := c.Discover(ctx, targetFromProject(p, []Environment{e}))
		if err != nil {
			return nil, err
		}
		return res.Vars, nil
	}
	return nil, fmt.Errorf("railway: environment %s not found in project %s", envID, projectID)
}

func describeAlias(t DiscoveryTarget, a *discoveryAlias) string {
	if a == nil {
		return t.ProjectName
	}
	envName := a.envID
	for _, e := range t.Environments {
		if e.ID == a.envID {
			envName = e.Name
		}
	}
	if a.timestamps || a.serviceID == "" {
		return t.ProjectName + "/" + envName
	}
	svc := t.ServiceNames[a.serviceID]
	if svc == "" {
		svc = a.serviceID
	}
	return t.ProjectName + "/" + envName + "/" + svc
}

func (c *Client) collectTimestamps(ctx context.Context, envID string, first *timestampPage) ([]timestampNode, error) {
	if first == nil {
		return nil, nil
	}
	nodes := first.nodes()
	page := first
	for page.PageInfo.HasNextPage && page.PageInfo.EndCursor != "" {
		var d struct {
			Environment struct {
				Variables timestampPage `json:"variables"`
			} `json:"environment"`
		}
		vars := map[string]any{"environmentId": envID, "after": page.PageInfo.EndCursor}
		if err := c.T.Do(ctx, "VariablesPage", variablesPageDoc, vars, &d); err != nil {
			return nil, err
		}
		page = &d.Environment.Variables
		nodes = append(nodes, page.nodes()...)
	}
	return nodes, nil
}

// Discover reads every variable of one project: one request, plus further
// timestamp pages. "not found" partial errors are dropped; others become
// warnings `project/env/service: message`.
func (c *Client) Discover(ctx context.Context, t DiscoveryTarget) (DiscoveryResult, error) {
	res := DiscoveryResult{ProjectID: t.ProjectID}
	for _, env := range t.Environments {
		if !env.CanAccess {
			res.Warnings = append(res.Warnings, t.ProjectName+"/"+env.Name+": no access")
		} else if len(env.ServiceIDs) == 0 {
			res.Warnings = append(res.Warnings, t.ProjectName+"/"+env.Name+": no services")
		}
	}
	req := buildDiscoveryRequest(t)
	if len(req.aliases) == 0 {
		return res, nil
	}

	data := map[string]json.RawMessage{}
	err := c.T.Do(ctx, "Discover", req.document, req.variables, &data)
	var gqe *GraphQLError
	if err != nil && !errors.As(err, &gqe) {
		return res, err
	}
	if gqe != nil {
		hasData := false
		for _, raw := range data {
			if len(raw) > 0 && string(raw) != "null" {
				hasData = true
			}
		}
		for _, e := range gqe.Errors {
			if notFound.MatchString(e.Message) {
				continue
			}
			if !hasData {
				// The whole request failed: not "the project is empty".
				return res, gqe
			}
			var a *discoveryAlias
			if len(e.Path) > 0 {
				if name, ok := e.Path[0].(string); ok {
					if v, ok := req.aliases[name]; ok {
						a = &v
					}
				}
			}
			why := describeAlias(t, a) + ": " + e.Message
			res.Warnings = append(res.Warnings, why)
			if a != nil && !a.timestamps && !slices.ContainsFunc(res.Unread, func(u UnreadPlace) bool {
				return u.EnvID == a.envID && u.ServiceID == a.serviceID
			}) {
				res.Unread = append(res.Unread, UnreadPlace{EnvID: a.envID, ServiceID: a.serviceID, Why: why})
			}
		}
	}

	findVars := func(envID, serviceID string, rendered bool) map[string]*string {
		for alias, a := range req.aliases {
			if a.timestamps || a.envID != envID || a.serviceID != serviceID || a.rendered != rendered {
				continue
			}
			raw := data[alias]
			if len(raw) == 0 || string(raw) == "null" {
				return nil
			}
			var m map[string]*string
			if json.Unmarshal(raw, &m) != nil {
				return nil
			}
			return m
		}
		return nil
	}

	for _, env := range t.Environments {
		if !env.CanAccess {
			continue
		}
		var first *timestampPage
		for alias, a := range req.aliases {
			if !a.timestamps || a.envID != env.ID {
				continue
			}
			var w struct {
				Variables *timestampPage `json:"variables"`
			}
			if raw := data[alias]; len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &w); err != nil {
					return res, fmt.Errorf("decode Railway data: %w", err)
				}
			}
			first = w.Variables
		}
		nodes, err := c.collectTimestamps(ctx, env.ID, first)
		if err != nil {
			return res, err
		}
		byKey := make(map[string]timestampNode, len(nodes))
		for _, n := range nodes {
			byKey[n.ServiceID+"/"+n.Name] = n
		}

		for _, sid := range append([]string{""}, env.ServiceIDs...) {
			rendered := findVars(env.ID, sid, true)
			unrendered := findVars(env.ID, sid, false)
			for _, name := range SystemVarNames {
				v := rendered[name]
				if v == nil || *v == "" {
					continue
				}
				// Railway's own values are only among the rendered ones; one
				// also among the unrendered is a user's, and anyone who can
				// set variables could point a database tunnel elsewhere with it.
				if _, user := unrendered[name]; user {
					res.Warnings = append(res.Warnings, describeAlias(t, &discoveryAlias{envID: env.ID, serviceID: sid})+": a variable named "+name+" shadows Railway's; not used")
					continue
				}
				res.System = append(res.System, SystemRecord{EnvID: env.ID, ServiceID: sid, Name: name, Value: *v})
			}
			if len(unrendered) == 0 {
				continue
			}
			for name, v := range unrendered {
				val := ""
				if v != nil { // a sealed value comes as null
					val = *v
				}
				rv := val
				if r := rendered[name]; r != nil {
					rv = *r
				}
				rec := VarRecord{EnvID: env.ID, ServiceID: sid, Name: name, Unrendered: val, Rendered: rv, Sealed: v == nil}
				if n, ok := byKey[sid+"/"+name]; ok {
					if len(n.UpdatedAt) >= 19 {
						rec.UpdatedAt = n.UpdatedAt[:19]
					}
					rec.Sealed = rec.Sealed || n.IsSealed
				}
				res.Vars = append(res.Vars, rec)
			}
		}
	}
	return res, nil
}

// DiscoverAll discovers the targets, up to 8 projects at a time. each is
// called once per target, never concurrently, in completion order.
func (c *Client) DiscoverAll(ctx context.Context, ts []DiscoveryTarget, each func(DiscoveryResult, error)) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, discoveryConcurrency)
	for _, t := range ts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := c.Discover(ctx, t)
			mu.Lock()
			defer mu.Unlock()
			each(res, err)
		}()
	}
	wg.Wait()
}
