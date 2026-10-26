package railway

import (
	"fmt"
	"strings"
)

// DefaultEndpoint is Railway's GraphQL endpoint.
const DefaultEndpoint = "https://backboard.railway.com/graphql/v2"

const (
	whoAmIDoc = `query WhoAmI { me { id name email } }`

	listWorkspacesDoc = `query ListWorkspaces { me { workspaces { id name } } }`

	workspaceProjectsDoc = `query WorkspaceProjects($workspaceId: String!, $after: String) {
  workspace(workspaceId: $workspaceId) {
    projects(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      edges { node {
        id name
        environments { edges { node { id name canAccess serviceInstances { edges { node { serviceId } } } } } }
        services { edges { node { id name } } }
      } }
    }
  }
}`

	// projectEnvironmentDoc is the catalog query of one project (DiscoverEnv).
	projectEnvironmentDoc = `query ProjectEnvironment($projectId: String!) {
  project(id: $projectId) {
    id name
    environments { edges { node { id name canAccess serviceInstances { edges { node { serviceId } } } } } }
    services { edges { node { id name } } }
  }
}`

	timestampSelection = `pageInfo { hasNextPage endCursor } edges { node { name serviceId updatedAt isSealed } }`

	variablesPageDoc = `query VariablesPage($environmentId: String!, $after: String) {
  environment(id: $environmentId) { variables(first: 500, after: $after) { ` + timestampSelection + ` } }
}`

	readVariablesDoc = `query ReadVariables($projectId: String!, $environmentId: String!, $serviceId: String, $unrendered: Boolean) {
  variables(projectId: $projectId, environmentId: $environmentId, serviceId: $serviceId, unrendered: $unrendered)
}`

	// Changes to one environment's variables as one patch, never deploying.
	commitPatchDoc = `mutation CommitPatch($environmentId: String!, $patch: EnvironmentConfig!, $message: String!) {
  environmentPatchCommit(environmentId: $environmentId, patch: $patch, skipDeploys: true, commitMessage: $message)
}`

	patchStatusDoc = `query PatchStatus($id: String!) {
  environmentPatch(id: $id) { id status lastAppliedError }
}`

	redeployDoc = `mutation Redeploy($environmentId: String!, $serviceId: String!) {
  serviceInstanceRedeploy(environmentId: $environmentId, serviceId: $serviceId)
}`

	serviceDeploymentsDoc = `query ServiceDeployments($serviceId: String!, $environmentId: String!) {
  deployments(first: 10, input: { serviceId: $serviceId, environmentId: $environmentId }) { edges { node { id status createdAt } } }
}`

	activeDeploymentDoc = `query ActiveDeployment($environmentId: String!, $serviceId: String!) {
  serviceInstance(environmentId: $environmentId, serviceId: $serviceId) { activeDeployments { id } }
}`

	restartDeploymentDoc = `mutation RestartDeployment($id: String!) {
  deploymentRestart(id: $id)
}`
)

type discoveryAlias struct {
	timestamps bool
	envID      string
	serviceID  string // "" = shared
	rendered   bool
}

type discoveryRequest struct {
	document  string
	variables map[string]any
	aliases   map[string]discoveryAlias
}

// buildDiscoveryRequest builds one request per project: per accessible
// environment the first timestamp page, the shared variables (unrendered and
// rendered) and, per deployed service, its variables (unrendered and
// rendered). Ids travel as GraphQL variables.
func buildDiscoveryRequest(t DiscoveryTarget) discoveryRequest {
	vars := map[string]any{"p": t.ProjectID}
	decls := []string{"$p: String!"}
	var fields []string
	aliases := map[string]discoveryAlias{}

	for i, env := range t.Environments {
		if !env.CanAccess {
			continue
		}
		e := fmt.Sprintf("e%d", i)
		vars[e] = env.ID
		decls = append(decls, fmt.Sprintf("$%s: String!", e))

		fields = append(fields, fmt.Sprintf("t%d: environment(id: $%s) { variables(first: 500) { %s } }", i, e, timestampSelection))
		aliases[fmt.Sprintf("t%d", i)] = discoveryAlias{timestamps: true, envID: env.ID}

		fields = append(fields,
			fmt.Sprintf("sh%du: variables(projectId: $p, environmentId: $%s, unrendered: true)", i, e),
			fmt.Sprintf("sh%dr: variables(projectId: $p, environmentId: $%s)", i, e))
		aliases[fmt.Sprintf("sh%du", i)] = discoveryAlias{envID: env.ID}
		aliases[fmt.Sprintf("sh%dr", i)] = discoveryAlias{envID: env.ID, rendered: true}

		for j, sid := range env.ServiceIDs {
			s := fmt.Sprintf("s%d_%d", i, j)
			vars[s] = sid
			decls = append(decls, fmt.Sprintf("$%s: String!", s))
			fields = append(fields,
				fmt.Sprintf("%su: variables(projectId: $p, environmentId: $%s, serviceId: $%s, unrendered: true)", s, e, s),
				fmt.Sprintf("%sr: variables(projectId: $p, environmentId: $%s, serviceId: $%s)", s, e, s))
			aliases[s+"u"] = discoveryAlias{envID: env.ID, serviceID: sid}
			aliases[s+"r"] = discoveryAlias{envID: env.ID, serviceID: sid, rendered: true}
		}
	}
	doc := fmt.Sprintf("query Discover(%s) {\n  %s\n}", strings.Join(decls, ", "), strings.Join(fields, "\n  "))
	return discoveryRequest{document: doc, variables: vars, aliases: aliases}
}

// Monitor reads (Services screen) and environment config.
const (
	// serviceStatesDoc: per service instance the running and the newest
	// deployment; and the project's deployments in progress.
	serviceStatesDoc = `query ServiceStates($projectId: String!) {
  project(id: $projectId) { environments { edges { node { id serviceInstances { edges { node {
    serviceId activeDeployments { id status } latestDeployment { id status }
  } } } } } } }
  deployments(first: 100, input: { projectId: $projectId, status: { in: [QUEUED, WAITING, INITIALIZING, BUILDING, DEPLOYING] } }) {
    edges { node { id status serviceId environmentId } }
  }
}`

	// serviceMetricsDoc: every service of a project, by service and
	// environment.
	serviceMetricsDoc = `query ServiceMetrics($projectId: String!, $startDate: DateTime!, $sampleRateSeconds: Int!) {
  metrics(
    projectId: $projectId, startDate: $startDate, sampleRateSeconds: $sampleRateSeconds,
    averagingWindowSeconds: $sampleRateSeconds, groupBy: [SERVICE_ID, ENVIRONMENT_ID],
    measurements: [CPU_USAGE, MEMORY_USAGE_GB, NETWORK_RX_GB, NETWORK_TX_GB, CPU_LIMIT, MEMORY_LIMIT_GB]
  ) { measurement tags { serviceId environmentId } values { ts value } }
}`

	// serviceMetricsOfDoc: one service instance (the panel).
	serviceMetricsOfDoc = `query ServiceMetricsOf($environmentId: String!, $serviceId: String!, $startDate: DateTime!, $sampleRateSeconds: Int!) {
  metrics(
    environmentId: $environmentId, serviceId: $serviceId, startDate: $startDate, sampleRateSeconds: $sampleRateSeconds,
    averagingWindowSeconds: $sampleRateSeconds,
    measurements: [CPU_USAGE, MEMORY_USAGE_GB, NETWORK_RX_GB, NETWORK_TX_GB, CPU_LIMIT, MEMORY_LIMIT_GB]
  ) { measurement tags { serviceId environmentId } values { ts value } }
}`

	serviceVolumesDoc = `query ServiceVolumes($projectId: String!) {
  project(id: $projectId) { volumes { edges { node { volumeInstances { edges { node {
    environmentId serviceId mountPath sizeMB currentSizeMB volume { id }
  } } } } } } }
}`

	// serviceDetailDoc: settings of one instance, the running deployment's meta
	// (regions) and the three newest deployments.
	serviceDetailDoc = `query ServiceDetail($environmentId: String!, $serviceId: String!) {
  serviceInstance(environmentId: $environmentId, serviceId: $serviceId) {
    region numReplicas healthcheckPath sleepApplication restartPolicyType restartPolicyMaxRetries
    cronSchedule nextCronRunAt builder source { image repo }
    activeDeployments { id meta }
  }
  deployments(first: 3, input: { environmentId: $environmentId, serviceId: $serviceId }) {
    edges { node { id status createdAt creator { name } meta } }
  }
}`

	domainsDoc = `query Domains($projectId: String!, $environmentId: String!, $serviceId: String!) {
  domains(projectId: $projectId, environmentId: $environmentId, serviceId: $serviceId) {
    serviceDomains { id domain targetPort }
    customDomains { id domain targetPort status { verified certificateStatus } }
  }
}`

	environmentConfigDoc = `query EnvironmentConfig($environmentId: String!) {
  environment(id: $environmentId) { configEtag config(decryptVariables: false) }
}`

	serviceDomainCreateDoc = `mutation ServiceDomainCreate($input: ServiceDomainCreateInput!) {
  serviceDomainCreate(input: $input) { id domain targetPort }
}`

	serviceDomainDeleteDoc = `mutation ServiceDomainDelete($id: String!) {
  serviceDomainDelete(id: $id)
}`

	customDomainCreateDoc = `mutation CustomDomainCreate($input: CustomDomainCreateInput!) {
  customDomainCreate(input: $input) { id domain targetPort status { verified certificateStatus } }
}`

	customDomainDeleteDoc = `mutation CustomDomainDelete($id: String!) {
  customDomainDelete(id: $id)
}`
)

// logSelection is what a log line carries, in the query and the
// subscription alike.
const logSelection = `{ timestamp message severity }`

// deploymentLogsDoc reads a deployment's newest lines.
const deploymentLogsDoc = `query DeploymentLogs($deploymentId: String!, $limit: Int) {
  deploymentLogs(deploymentId: $deploymentId, limit: $limit) ` + logSelection + `
}`

// DeploymentLogsSubscription streams a deployment's log lines (variables
// deploymentId and limit); each payload's data decodes with
// DecodeDeploymentLogs.
const DeploymentLogsSubscription = `subscription DeploymentLogs($deploymentId: String!, $limit: Int) {
  deploymentLogs(deploymentId: $deploymentId, limit: $limit) ` + logSelection + `
}`

// History reads. The shapes were checked against the live API.
const (
	// environmentHistoryDoc: one page of an environment's history, every
	// object type. `payload` is decoded only for its `names`.
	environmentHistoryDoc = `query EnvironmentHistory($environmentId: String!, $after: String, $since: String) {
  environmentHistory(environmentId: $environmentId, first: 100, after: $after, filter: { since: $since }) {
    pageInfo { hasNextPage endCursor }
    edges { node { id object action createdAt serviceIds payload workflowId actor { principal } } }
  }
}`

	// environmentPatchesDoc: one page of an environment's patches, newest
	// first, with their commit messages.
	environmentPatchesDoc = `query EnvironmentPatches($environmentId: String!, $after: String) {
  environmentPatches(environmentId: $environmentId, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    edges { node { id message createdAt } }
  }
}`

	// projectMembersDoc reads the workspace's members: projectMembers answers
	// none for a project in a workspace.
	projectMembersDoc = `query ProjectMembers($projectId: String!) {
  project(id: $projectId) { workspace { members { id name email } } }
}`
)
