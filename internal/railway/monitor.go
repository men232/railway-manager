package railway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

func isPartial(err error) bool {
	var p *PartialError
	return errors.As(err, &p)
}

// PartialError accompanies usable data: the request answered some of its
// fields and failed others (an inaccessible environment or service). The
// project-wide reads (ServiceStates, ServiceMetrics, ServiceVolumes) return it
// together with non-empty data, so callers use the data and may show the
// warnings (check with errors.As). "not found" messages are dropped, as
// Discover does. When no data came back the error is the *GraphQLError itself.
type PartialError struct{ Warnings []string }

func (e *PartialError) Error() string {
	return "railway: partial answer: " + strings.Join(e.Warnings, "; ")
}

// doPartial runs a project-wide read, tolerating partial data. hasData
// reports whether the decoded answer holds anything.
func (c *Client) doPartial(ctx context.Context, op, doc string, vars map[string]any, dst any, hasData func() bool) error {
	err := c.T.Do(ctx, op, doc, vars, dst)
	var gqe *GraphQLError
	if err == nil || !errors.As(err, &gqe) {
		return err
	}
	var warnings []string
	for _, e := range gqe.Errors {
		if !notFound.MatchString(e.Message) {
			warnings = append(warnings, e.Message)
		}
	}
	if len(warnings) == 0 {
		return nil
	}
	if !hasData() {
		return gqe
	}
	return &PartialError{Warnings: warnings}
}

// minSampleSeconds is Railway's finest metrics sample rate.
const minSampleSeconds = 30

// DeploymentState is one service instance's deployments: the running one,
// the newest one and the ids of those in progress.
type DeploymentState struct {
	EnvID, ServiceID, ActiveID, ActiveStatus, LatestStatus string
	InProgress                                             []string
}

// ServiceStates reads every service instance of a project. Partial data comes
// with a *PartialError.
func (c *Client) ServiceStates(ctx context.Context, projectID string) ([]DeploymentState, error) {
	type ref struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	var d struct {
		Project struct {
			Environments edges[struct {
				ID               string `json:"id"`
				ServiceInstances edges[struct {
					ServiceID         string `json:"serviceId"`
					ActiveDeployments []ref  `json:"activeDeployments"`
					LatestDeployment  *ref   `json:"latestDeployment"`
				}] `json:"serviceInstances"`
			}] `json:"environments"`
		} `json:"project"`
		Deployments edges[struct {
			ID            string `json:"id"`
			ServiceID     string `json:"serviceId"`
			EnvironmentID string `json:"environmentId"`
		}] `json:"deployments"`
	}
	perr := c.doPartial(ctx, "ServiceStates", serviceStatesDoc, map[string]any{"projectId": projectID}, &d, func() bool {
		return len(d.Project.Environments.Edges) > 0 || len(d.Deployments.Edges) > 0
	})
	if perr != nil && !isPartial(perr) {
		return nil, perr
	}
	building := d.Deployments.nodes()
	var out []DeploymentState
	for _, env := range d.Project.Environments.nodes() {
		for _, si := range env.ServiceInstances.nodes() {
			st := DeploymentState{EnvID: env.ID, ServiceID: si.ServiceID}
			if len(si.ActiveDeployments) > 0 {
				st.ActiveID, st.ActiveStatus = si.ActiveDeployments[0].ID, si.ActiveDeployments[0].Status
			}
			if si.LatestDeployment != nil {
				st.LatestStatus = si.LatestDeployment.Status
			}
			for _, b := range building {
				if b.EnvironmentID == env.ID && b.ServiceID == si.ServiceID {
					st.InProgress = append(st.InProgress, b.ID)
				}
			}
			out = append(out, st)
		}
	}
	return out, perr
}

// MetricPoint is one sample: Unix seconds and value.
type MetricPoint struct {
	TS    int64
	Value float64
}

// MetricSeries is one measurement of one service instance.
type MetricSeries struct {
	EnvID, ServiceID, Measurement string
	Points                        []MetricPoint
}

type metricsAnswer struct {
	Metrics []struct {
		Measurement string `json:"measurement"`
		Tags        struct {
			ServiceID     *string `json:"serviceId"`
			EnvironmentID *string `json:"environmentId"`
		} `json:"tags"`
		Values []struct {
			TS    int64   `json:"ts"`
			Value float64 `json:"value"`
		} `json:"values"`
	} `json:"metrics"`
}

func (a metricsAnswer) series(envID, serviceID string) []MetricSeries {
	var out []MetricSeries
	for _, m := range a.Metrics {
		e, s := envID, serviceID
		if e == "" || s == "" {
			if m.Tags.EnvironmentID == nil || m.Tags.ServiceID == nil || *m.Tags.EnvironmentID == "" || *m.Tags.ServiceID == "" {
				continue
			}
			e, s = *m.Tags.EnvironmentID, *m.Tags.ServiceID
		}
		ms := MetricSeries{EnvID: e, ServiceID: s, Measurement: m.Measurement}
		for _, v := range m.Values {
			ms.Points = append(ms.Points, MetricPoint{TS: v.TS, Value: v.Value})
		}
		out = append(out, ms)
	}
	return out
}

// ServiceMetrics reads every service's metrics of a project since `since`,
// one point per sampleSeconds (clamped to 30, Railway's finest). Partial data
// comes with a *PartialError.
func (c *Client) ServiceMetrics(ctx context.Context, projectID string, since time.Time, sampleSeconds int) ([]MetricSeries, error) {
	vars := map[string]any{"projectId": projectID, "startDate": since.UTC().Format(time.RFC3339), "sampleRateSeconds": max(sampleSeconds, minSampleSeconds)}
	var d metricsAnswer
	perr := c.doPartial(ctx, "ServiceMetrics", serviceMetricsDoc, vars, &d, func() bool { return len(d.Metrics) > 0 })
	if perr != nil && !isPartial(perr) {
		return nil, perr
	}
	return d.series("", ""), perr
}

// ServiceMetricsOf reads the same measurements for one service instance
// (sampleSeconds clamped to 30).
func (c *Client) ServiceMetricsOf(ctx context.Context, envID, serviceID string, since time.Time, sampleSeconds int) ([]MetricSeries, error) {
	vars := map[string]any{"environmentId": envID, "serviceId": serviceID, "startDate": since.UTC().Format(time.RFC3339), "sampleRateSeconds": max(sampleSeconds, minSampleSeconds)}
	var d metricsAnswer
	if err := c.T.Do(ctx, "ServiceMetricsOf", serviceMetricsOfDoc, vars, &d); err != nil {
		return nil, err
	}
	return d.series(envID, serviceID), nil
}

// VolumeMount is a volume instance mounted in a service. Sizes are MB.
type VolumeMount struct {
	EnvID, ServiceID, VolumeID, MountPath string
	SizeMB, UsedMB                        float64
}

// ServiceVolumes reads every volume instance of a project that is mounted in a
// service. Partial data comes with a *PartialError.
func (c *Client) ServiceVolumes(ctx context.Context, projectID string) ([]VolumeMount, error) {
	var d struct {
		Project struct {
			Volumes edges[struct {
				VolumeInstances edges[struct {
					EnvironmentID string  `json:"environmentId"`
					ServiceID     *string `json:"serviceId"`
					MountPath     string  `json:"mountPath"`
					SizeMB        float64 `json:"sizeMB"`
					CurrentSizeMB float64 `json:"currentSizeMB"`
					Volume        *struct {
						ID string `json:"id"`
					} `json:"volume"`
				}] `json:"volumeInstances"`
			}] `json:"volumes"`
		} `json:"project"`
	}
	perr := c.doPartial(ctx, "ServiceVolumes", serviceVolumesDoc, map[string]any{"projectId": projectID}, &d, func() bool {
		return len(d.Project.Volumes.Edges) > 0
	})
	if perr != nil && !isPartial(perr) {
		return nil, perr
	}
	var out []VolumeMount
	for _, v := range d.Project.Volumes.nodes() {
		for _, vi := range v.VolumeInstances.nodes() {
			if vi.ServiceID == nil || *vi.ServiceID == "" {
				continue
			}
			m := VolumeMount{EnvID: vi.EnvironmentID, ServiceID: *vi.ServiceID, MountPath: vi.MountPath, SizeMB: vi.SizeMB, UsedMB: vi.CurrentSizeMB}
			if vi.Volume != nil {
				m.VolumeID = vi.Volume.ID
			}
			out = append(out, m)
		}
	}
	return out, perr
}

// DeployInfo is one deployment of the detail panel.
type DeployInfo struct {
	ID, Status, Reason, Creator, CommitHash, CommitMessage string
	CreatedAt                                              time.Time
}

// ServiceDetail is one service instance's settings and newest deployments.
type ServiceDetail struct {
	Regions                                                        map[string]int
	Repo, Image, Branch, Builder, RestartPolicy, Cron, Healthcheck string
	Sleeps                                                         bool
	RestartMaxRetries                                              int
	NextCron                                                       time.Time
	Deploys                                                        []DeployInfo
}

type deployMeta struct {
	Reason          string `json:"reason"`
	CommitHash      string `json:"commitHash"`
	CommitMessage   string `json:"commitMessage"`
	Branch          string `json:"branch"`
	ServiceManifest struct {
		Deploy struct {
			MultiRegionConfig map[string]*struct {
				NumReplicas *float64 `json:"numReplicas"`
			} `json:"multiRegionConfig"`
		} `json:"deploy"`
	} `json:"serviceManifest"`
}

// parseMeta decodes a deployment's meta; a malformed or missing one is empty.
// Fields of the wrong type are skipped one by one.
func parseMeta(raw json.RawMessage) deployMeta {
	var m deployMeta
	if len(raw) == 0 || string(raw) == "null" {
		return m
	}
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var loose map[string]json.RawMessage
	if json.Unmarshal(raw, &loose) != nil {
		return deployMeta{}
	}
	for k, dst := range map[string]*string{"reason": &m.Reason, "commitHash": &m.CommitHash, "commitMessage": &m.CommitMessage, "branch": &m.Branch} {
		_ = json.Unmarshal(loose[k], dst)
	}
	_ = json.Unmarshal(loose["serviceManifest"], &m.ServiceManifest)
	return m
}

// ServiceDetail reads one service instance's settings and its three newest deployments.
func (c *Client) ServiceDetail(ctx context.Context, envID, serviceID string) (ServiceDetail, error) {
	var d struct {
		ServiceInstance *struct {
			Region            *string `json:"region"`
			NumReplicas       *int    `json:"numReplicas"`
			HealthcheckPath   *string `json:"healthcheckPath"`
			SleepApplication  *bool   `json:"sleepApplication"`
			RestartPolicyType *string `json:"restartPolicyType"`
			RestartMaxRetries *int    `json:"restartPolicyMaxRetries"`
			CronSchedule      *string `json:"cronSchedule"`
			NextCronRunAt     *string `json:"nextCronRunAt"`
			Builder           *string `json:"builder"`
			Source            *struct {
				Image *string `json:"image"`
				Repo  *string `json:"repo"`
			} `json:"source"`
			ActiveDeployments []struct {
				Meta json.RawMessage `json:"meta"`
			} `json:"activeDeployments"`
		} `json:"serviceInstance"`
		Deployments edges[struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			CreatedAt string `json:"createdAt"`
			Creator   *struct {
				Name *string `json:"name"`
			} `json:"creator"`
			Meta json.RawMessage `json:"meta"`
		}] `json:"deployments"`
	}
	vars := map[string]any{"environmentId": envID, "serviceId": serviceID}
	if err := c.T.Do(ctx, "ServiceDetail", serviceDetailDoc, vars, &d); err != nil {
		return ServiceDetail{}, err
	}
	var out ServiceDetail
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if si := d.ServiceInstance; si != nil {
		out.Healthcheck, out.RestartPolicy, out.Cron, out.Builder = str(si.HealthcheckPath), str(si.RestartPolicyType), str(si.CronSchedule), str(si.Builder)
		out.Sleeps = si.SleepApplication != nil && *si.SleepApplication
		if si.RestartMaxRetries != nil {
			out.RestartMaxRetries = *si.RestartMaxRetries
		}
		if si.NextCronRunAt != nil {
			out.NextCron, _ = time.Parse(time.RFC3339Nano, *si.NextCronRunAt)
		}
		if si.Source != nil {
			out.Repo, out.Image = str(si.Source.Repo), str(si.Source.Image)
		}
		if len(si.ActiveDeployments) > 0 {
			for r, cfg := range parseMeta(si.ActiveDeployments[0].Meta).ServiceManifest.Deploy.MultiRegionConfig {
				if out.Regions == nil {
					out.Regions = map[string]int{}
				}
				n := 1
				if cfg != nil && cfg.NumReplicas != nil {
					n = int(*cfg.NumReplicas)
				}
				out.Regions[r] = n
			}
		}
		if out.Regions == nil && si.Region != nil && *si.Region != "" {
			n := 1
			if si.NumReplicas != nil {
				n = *si.NumReplicas
			}
			out.Regions = map[string]int{*si.Region: n}
		}
	}
	for i, n := range d.Deployments.nodes() {
		meta := parseMeta(n.Meta)
		if i == 0 {
			out.Branch = meta.Branch
		}
		di := DeployInfo{ID: n.ID, Status: n.Status, Reason: meta.Reason, CommitHash: meta.CommitHash,
			CommitMessage: strings.SplitN(meta.CommitMessage, "\n", 2)[0]}
		di.CreatedAt, _ = time.Parse(time.RFC3339Nano, n.CreatedAt)
		if n.Creator != nil {
			di.Creator = str(n.Creator.Name)
		}
		out.Deploys = append(out.Deploys, di)
	}
	return out, nil
}

type ServiceDomain struct {
	ID, Domain string
	Port       int
}

type CustomDomain struct {
	ID, Domain string
	Port       int
	Verified   bool
	CertStatus string
}

type Domains struct {
	Service []ServiceDomain
	Custom  []CustomDomain
}

// Domains reads the service and custom domains of one service instance.
func (c *Client) Domains(ctx context.Context, projectID, envID, serviceID string) (Domains, error) {
	var d struct {
		Domains struct {
			ServiceDomains []struct {
				ID         string `json:"id"`
				Domain     string `json:"domain"`
				TargetPort *int   `json:"targetPort"`
			} `json:"serviceDomains"`
			CustomDomains []struct {
				ID         string `json:"id"`
				Domain     string `json:"domain"`
				TargetPort *int   `json:"targetPort"`
				Status     *struct {
					Verified          bool    `json:"verified"`
					CertificateStatus *string `json:"certificateStatus"`
				} `json:"status"`
			} `json:"customDomains"`
		} `json:"domains"`
	}
	vars := map[string]any{"projectId": projectID, "environmentId": envID, "serviceId": serviceID}
	if err := c.T.Do(ctx, "Domains", domainsDoc, vars, &d); err != nil {
		return Domains{}, err
	}
	port := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	var out Domains
	for _, s := range d.Domains.ServiceDomains {
		out.Service = append(out.Service, ServiceDomain{ID: s.ID, Domain: s.Domain, Port: port(s.TargetPort)})
	}
	for _, s := range d.Domains.CustomDomains {
		cd := CustomDomain{ID: s.ID, Domain: s.Domain, Port: port(s.TargetPort)}
		if s.Status != nil {
			cd.Verified = s.Status.Verified
			if s.Status.CertificateStatus != nil {
				cd.CertStatus = *s.Status.CertificateStatus
			}
		}
		out.Custom = append(out.Custom, cd)
	}
	return out, nil
}

// ServiceConfig is one service's limits and replicas as the config states
// them; nil/empty means no override.
type ServiceConfig struct {
	CPU         *float64
	MemoryBytes *int64
	Replicas    map[string]int
}

type VolumeConfig struct {
	SizeMB            int
	AllowOnlineResize bool
}

// EnvConfig is the part of an environment's config the app edits, with the
// config etag (changes whenever the config does).
type EnvConfig struct {
	Etag     string
	Services map[string]ServiceConfig
	Volumes  map[string]VolumeConfig
}

// EnvironmentConfig reads an environment's config, keeping only
// services.<id>.deploy.limitOverride.containers, ...deploy.multiRegionConfig.<region>.numReplicas
// and volumes.<id>.{sizeMB, allowOnlineResize}.
func (c *Client) EnvironmentConfig(ctx context.Context, envID string) (EnvConfig, error) {
	var d struct {
		Environment *struct {
			Etag   string `json:"configEtag"`
			Config struct {
				Services map[string]*struct {
					Deploy *struct {
						LimitOverride *struct {
							Containers *struct {
								CPU         *float64 `json:"cpu"`
								MemoryBytes *float64 `json:"memoryBytes"`
							} `json:"containers"`
						} `json:"limitOverride"`
						MultiRegionConfig map[string]*struct {
							NumReplicas *float64 `json:"numReplicas"`
						} `json:"multiRegionConfig"`
					} `json:"deploy"`
				} `json:"services"`
				Volumes map[string]*struct {
					SizeMB            *float64 `json:"sizeMB"`
					AllowOnlineResize *bool    `json:"allowOnlineResize"`
				} `json:"volumes"`
			} `json:"config"`
		} `json:"environment"`
	}
	if err := c.T.Do(ctx, "EnvironmentConfig", environmentConfigDoc, map[string]any{"environmentId": envID}, &d); err != nil {
		return EnvConfig{}, err
	}
	out := EnvConfig{Services: map[string]ServiceConfig{}, Volumes: map[string]VolumeConfig{}}
	if d.Environment == nil {
		return out, nil
	}
	out.Etag = d.Environment.Etag
	for id, s := range d.Environment.Config.Services {
		if s == nil || s.Deploy == nil {
			continue
		}
		var sc ServiceConfig
		if lo := s.Deploy.LimitOverride; lo != nil && lo.Containers != nil {
			sc.CPU = lo.Containers.CPU
			if m := lo.Containers.MemoryBytes; m != nil {
				v := int64(*m)
				sc.MemoryBytes = &v
			}
		}
		for region, r := range s.Deploy.MultiRegionConfig {
			if r == nil || r.NumReplicas == nil {
				continue
			}
			if sc.Replicas == nil {
				sc.Replicas = map[string]int{}
			}
			sc.Replicas[region] = int(*r.NumReplicas)
		}
		if sc.CPU != nil || sc.MemoryBytes != nil || sc.Replicas != nil {
			out.Services[id] = sc
		}
	}
	for id, v := range d.Environment.Config.Volumes {
		if v == nil {
			continue
		}
		vc := VolumeConfig{}
		if v.SizeMB != nil {
			vc.SizeMB = int(*v.SizeMB)
		}
		vc.AllowOnlineResize = v.AllowOnlineResize != nil && *v.AllowOnlineResize
		out.Volumes[id] = vc
	}
	return out, nil
}

// LogLine is one line of a deployment's log, as Railway sent it: callers
// sanitize Message before showing it.
type LogLine struct {
	Timestamp         time.Time // zero when Railway's was unreadable
	Severity, Message string
}

type logNode struct {
	Timestamp string  `json:"timestamp"`
	Message   string  `json:"message"`
	Severity  *string `json:"severity"`
}

func logLines(nodes []logNode) []LogLine {
	out := make([]LogLine, 0, len(nodes))
	for _, n := range nodes {
		l := LogLine{Message: n.Message}
		if n.Severity != nil {
			l.Severity = *n.Severity
		}
		l.Timestamp, _ = time.Parse(time.RFC3339Nano, n.Timestamp)
		out = append(out, l)
	}
	slices.SortStableFunc(out, func(a, b LogLine) int { return a.Timestamp.Compare(b.Timestamp) })
	return out
}

// DeploymentLogs reads the newest limit lines of a deployment, oldest first.
func (c *Client) DeploymentLogs(ctx context.Context, deploymentID string, limit int) ([]LogLine, error) {
	var d struct {
		DeploymentLogs []logNode `json:"deploymentLogs"`
	}
	vars := map[string]any{"deploymentId": deploymentID, "limit": limit}
	if err := c.T.Do(ctx, "DeploymentLogs", deploymentLogsDoc, vars, &d); err != nil {
		return nil, err
	}
	return logLines(d.DeploymentLogs), nil
}

// DecodeDeploymentLogs decodes the data of one DeploymentLogsSubscription
// payload, oldest first.
func DecodeDeploymentLogs(data json.RawMessage) ([]LogLine, error) {
	var d struct {
		DeploymentLogs []logNode `json:"deploymentLogs"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("railway: deployment logs: %w", err)
	}
	return logLines(d.DeploymentLogs), nil
}
