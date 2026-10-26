package railway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Account is the signed-in Railway user.
type Account struct{ ID, Name, Email string }

// Catalog is every project the account can see.
type Catalog struct{ Projects []Project }

type Project struct {
	ID, Name, Workspace string
	Environments        []Environment
	Services            []Service
}

type Environment struct {
	ID, Name   string
	CanAccess  bool
	ServiceIDs []string
}

type Service struct{ ID, Name string }

// Deployment is one deployment of a service instance.
type Deployment struct {
	ID, Status string
	CreatedAt  time.Time
}

// WriteNotAllowedError: the write guard refused a mutation; nothing was sent.
type WriteNotAllowedError struct{ EnvID string }

func (e *WriteNotAllowedError) Error() string {
	return "writes to environment " + e.EnvID + " are not allowed"
}

// Client is the typed Railway API. WriteAllowed guards every mutation; a nil
// WriteAllowed allows no writes. Mutations are sent WithoutRetry: a step
// checks before it acts, so its own retry is the safe one.
type Client struct {
	T            *Transport
	WriteAllowed func(envID string) bool
}

// CheckWrite is the write guard every mutation passes: a WriteNotAllowedError
// for an environment outside WriteAllowed. A write that does not go through
// the API, such as a database password, asks it first.
func (c *Client) CheckWrite(envID string) error { return c.checkWrite(envID) }

func (c *Client) checkWrite(envID string) error {
	if c.WriteAllowed == nil || !c.WriteAllowed(envID) {
		return &WriteNotAllowedError{EnvID: envID}
	}
	return nil
}

func (c *Client) WhoAmI(ctx context.Context) (Account, error) {
	var d struct {
		Me Account `json:"me"`
	}
	if err := c.T.Do(ctx, "WhoAmI", whoAmIDoc, map[string]any{}, &d); err != nil {
		return Account{}, err
	}
	return d.Me, nil
}

type edges[T any] struct {
	Edges []struct {
		Node T `json:"node"`
	} `json:"edges"`
}

func (e edges[T]) nodes() []T {
	out := make([]T, len(e.Edges))
	for i, x := range e.Edges {
		out[i] = x.Node
	}
	return out
}

type projectNode struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Environments edges[struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		CanAccess        bool   `json:"canAccess"`
		ServiceInstances edges[struct {
			ServiceID string `json:"serviceId"`
		}] `json:"serviceInstances"`
	}] `json:"environments"`
	Services edges[serviceNode] `json:"services"`
}

// serviceNode decodes a service node.
type serviceNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (n projectNode) project(workspace string) Project {
	p := Project{ID: n.ID, Name: n.Name, Workspace: workspace}
	for _, e := range n.Environments.nodes() {
		env := Environment{ID: e.ID, Name: e.Name, CanAccess: e.CanAccess}
		for _, si := range e.ServiceInstances.nodes() {
			env.ServiceIDs = append(env.ServiceIDs, si.ServiceID)
		}
		p.Environments = append(p.Environments, env)
	}
	for _, s := range n.Services.nodes() {
		p.Services = append(p.Services, Service{ID: s.ID, Name: s.Name})
	}
	return p
}

// ListProjects returns every project of every workspace, all pages, by name.
func (c *Client) ListProjects(ctx context.Context) (Catalog, error) {
	var ws struct {
		Me struct {
			Workspaces []struct{ ID, Name string } `json:"workspaces"`
		} `json:"me"`
	}
	if err := c.T.Do(ctx, "ListWorkspaces", listWorkspacesDoc, map[string]any{}, &ws); err != nil {
		return Catalog{}, err
	}
	var cat Catalog
	for _, w := range ws.Me.Workspaces {
		var after any
		for {
			var page struct {
				Workspace struct {
					Projects struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						edges[projectNode]
					} `json:"projects"`
				} `json:"workspace"`
			}
			vars := map[string]any{"workspaceId": w.ID, "after": after}
			if err := c.T.Do(ctx, "WorkspaceProjects", workspaceProjectsDoc, vars, &page); err != nil {
				return Catalog{}, err
			}
			conn := page.Workspace.Projects
			for _, n := range conn.nodes() {
				cat.Projects = append(cat.Projects, n.project(w.Name))
			}
			if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
				break
			}
			after = conn.PageInfo.EndCursor
		}
	}
	sort.SliceStable(cat.Projects, func(i, j int) bool {
		return strings.ToLower(cat.Projects[i].Name) < strings.ToLower(cat.Projects[j].Name)
	})
	return cat, nil
}

// ReadVariables reads one scope's variables; serviceID "" is the shared scope.
// A sealed value comes back empty.
func (c *Client) ReadVariables(ctx context.Context, projectID, envID, serviceID string, unrendered bool) (map[string]string, error) {
	vars := map[string]any{"projectId": projectID, "environmentId": envID, "unrendered": unrendered}
	if serviceID != "" {
		vars["serviceId"] = serviceID
	}
	var d struct {
		Variables map[string]*string `json:"variables"`
	}
	if err := c.T.Do(ctx, "ReadVariables", readVariablesDoc, vars, &d); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(d.Variables))
	for k, v := range d.Variables {
		if v != nil {
			out[k] = *v
		} else {
			out[k] = ""
		}
	}
	return out, nil
}

// CommitPatch commits a patch of one environment without deploying and
// returns the patch id (the last `/`-segment of the answer
// `commitChanges/<envId>/<patchId>`).
func (c *Client) CommitPatch(ctx context.Context, envID string, patch map[string]any, message string) (string, error) {
	if err := c.checkWrite(envID); err != nil {
		return "", err
	}
	var d struct {
		Commit string `json:"environmentPatchCommit"`
	}
	vars := map[string]any{"environmentId": envID, "patch": patch, "message": message}
	if err := c.T.Do(WithoutRetry(ctx), "CommitPatch", commitPatchDoc, vars, &d); err != nil {
		return "", err
	}
	id := d.Commit[strings.LastIndex(d.Commit, "/")+1:]
	if id == "" {
		return "", fmt.Errorf("railway: patch commit answered no patch id")
	}
	return id, nil
}

// PatchStatus: APPLYING, then COMMITTED or FAILED with the error.
func (c *Client) PatchStatus(ctx context.Context, patchID string) (status, lastError string, err error) {
	var d struct {
		Patch struct {
			Status string  `json:"status"`
			Err    *string `json:"lastAppliedError"`
		} `json:"environmentPatch"`
	}
	if err := c.T.Do(ctx, "PatchStatus", patchStatusDoc, map[string]any{"id": patchID}, &d); err != nil {
		return "", "", err
	}
	if d.Patch.Err != nil {
		lastError = *d.Patch.Err
	}
	return d.Patch.Status, lastError, nil
}

// Redeploy redeploys a service instance with its current variables.
func (c *Client) Redeploy(ctx context.Context, envID, serviceID string) error {
	if err := c.checkWrite(envID); err != nil {
		return err
	}
	return c.T.Do(WithoutRetry(ctx), "Redeploy", redeployDoc, map[string]any{"environmentId": envID, "serviceId": serviceID}, nil)
}

// Deployments lists a service's newest deployments, newest first.
func (c *Client) Deployments(ctx context.Context, envID, serviceID string) ([]Deployment, error) {
	var d struct {
		Deployments edges[struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			CreatedAt string `json:"createdAt"`
		}] `json:"deployments"`
	}
	vars := map[string]any{"serviceId": serviceID, "environmentId": envID}
	if err := c.T.Do(ctx, "ServiceDeployments", serviceDeploymentsDoc, vars, &d); err != nil {
		return nil, err
	}
	var out []Deployment
	for _, n := range d.Deployments.nodes() {
		at, _ := time.Parse(time.RFC3339Nano, n.CreatedAt)
		out = append(out, Deployment{ID: n.ID, Status: n.Status, CreatedAt: at})
	}
	return out, nil
}

// ActiveDeployment is the id of the running deployment, "" when none.
func (c *Client) ActiveDeployment(ctx context.Context, envID, serviceID string) (string, error) {
	var d struct {
		ServiceInstance *struct {
			ActiveDeployments []struct {
				ID string `json:"id"`
			} `json:"activeDeployments"`
		} `json:"serviceInstance"`
	}
	vars := map[string]any{"environmentId": envID, "serviceId": serviceID}
	if err := c.T.Do(ctx, "ActiveDeployment", activeDeploymentDoc, vars, &d); err != nil {
		return "", err
	}
	if d.ServiceInstance == nil || len(d.ServiceInstance.ActiveDeployments) == 0 {
		return "", nil
	}
	return d.ServiceInstance.ActiveDeployments[0].ID, nil
}

// RestartDeployment restarts one deployment; envID is only for the write check.
func (c *Client) RestartDeployment(ctx context.Context, envID, deploymentID string) error {
	if err := c.checkWrite(envID); err != nil {
		return err
	}
	return c.T.Do(WithoutRetry(ctx), "RestartDeployment", restartDeploymentDoc, map[string]any{"id": deploymentID}, nil)
}
