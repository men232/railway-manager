package railway

import (
	"context"
	"errors"
)

// errDomainDeleteRefused is a delete Railway answered with false: nothing was
// removed, so the step must fail rather than go on to the read-back.
var errDomainDeleteRefused = errors.New("Railway refused to delete the domain")

func domainInput(envID, serviceID string, port int) map[string]any {
	in := map[string]any{"environmentId": envID, "serviceId": serviceID}
	if port != 0 {
		in["targetPort"] = port
	}
	return in
}

type domainNode struct {
	ID         string `json:"id"`
	Domain     string `json:"domain"`
	TargetPort *int   `json:"targetPort"`
}

func (n domainNode) port() int {
	if n.TargetPort == nil {
		return 0
	}
	return *n.TargetPort
}

// ServiceDomainCreate adds a Railway-provided domain; port 0 sends no
// targetPort.
func (c *Client) ServiceDomainCreate(ctx context.Context, envID, serviceID string, port int) (ServiceDomain, error) {
	if err := c.checkWrite(envID); err != nil {
		return ServiceDomain{}, err
	}
	var d struct {
		D domainNode `json:"serviceDomainCreate"`
	}
	vars := map[string]any{"input": domainInput(envID, serviceID, port)}
	if err := c.T.Do(WithoutRetry(ctx), "ServiceDomainCreate", serviceDomainCreateDoc, vars, &d); err != nil {
		return ServiceDomain{}, err
	}
	return ServiceDomain{ID: d.D.ID, Domain: d.D.Domain, Port: d.D.port()}, nil
}

// ServiceDomainDelete removes a Railway-provided domain; envID is the write
// guard's scope.
func (c *Client) ServiceDomainDelete(ctx context.Context, envID, id string) error {
	if err := c.checkWrite(envID); err != nil {
		return err
	}
	var d struct {
		OK bool `json:"serviceDomainDelete"`
	}
	if err := c.T.Do(WithoutRetry(ctx), "ServiceDomainDelete", serviceDomainDeleteDoc, map[string]any{"id": id}, &d); err != nil {
		return err
	}
	if !d.OK {
		return errDomainDeleteRefused
	}
	return nil
}

// CustomDomainCreate adds a custom domain; port 0 sends no targetPort.
func (c *Client) CustomDomainCreate(ctx context.Context, projectID, envID, serviceID, host string, port int) (CustomDomain, error) {
	if err := c.checkWrite(envID); err != nil {
		return CustomDomain{}, err
	}
	in := domainInput(envID, serviceID, port)
	in["projectId"] = projectID
	in["domain"] = host
	var d struct {
		D struct {
			domainNode
			Status *struct {
				Verified          bool   `json:"verified"`
				CertificateStatus string `json:"certificateStatus"`
			} `json:"status"`
		} `json:"customDomainCreate"`
	}
	if err := c.T.Do(WithoutRetry(ctx), "CustomDomainCreate", customDomainCreateDoc, map[string]any{"input": in}, &d); err != nil {
		return CustomDomain{}, err
	}
	out := CustomDomain{ID: d.D.ID, Domain: d.D.Domain, Port: d.D.port()}
	if s := d.D.Status; s != nil {
		out.Verified, out.CertStatus = s.Verified, s.CertificateStatus
	}
	return out, nil
}

// CustomDomainDelete removes a custom domain; envID is the write guard's scope.
func (c *Client) CustomDomainDelete(ctx context.Context, envID, id string) error {
	if err := c.checkWrite(envID); err != nil {
		return err
	}
	var d struct {
		OK bool `json:"customDomainDelete"`
	}
	if err := c.T.Do(WithoutRetry(ctx), "CustomDomainDelete", customDomainDeleteDoc, map[string]any{"id": id}, &d); err != nil {
		return err
	}
	if !d.OK {
		return errDomainDeleteRefused
	}
	return nil
}
