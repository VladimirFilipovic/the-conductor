package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// Dashboard reads: the flat meta lists the selectors are built from, and the
// topology tree the operator UI renders.

type metaJSON struct {
	Projects     []projectJSON     `json:"projects"`
	Environments []environmentJSON `json:"environments"`
	Regions      []string          `json:"regions"`
}

type projectJSON struct {
	Name string `json:"name"`
}

type environmentJSON struct {
	ID          uuid.UUID `json:"id"`
	ProjectName string    `json:"project_name"`
	Name        string    `json:"name"`
}

type topologyJSON struct {
	Tree   []projectNode `json:"tree"`
	Hosts  []hostJSON    `json:"hosts"`
	Served []servedJSON  `json:"served"`
}

// The tree: project → environment → service, each service carrying its
// current deployment, per-region desired-vs-observed, and its replicas.

type projectNode struct {
	Project      string            `json:"project"`
	Environments []environmentNode `json:"environments"`
}

type environmentNode struct {
	ID       uuid.UUID     `json:"id"`
	Name     string        `json:"name"`
	Services []serviceNode `json:"services"`
}

type serviceNode struct {
	EnvironmentServiceID uuid.UUID             `json:"environment_service_id"`
	EnvironmentID        uuid.UUID             `json:"environment_id"`
	Service              string                `json:"service"`
	Stateful             bool                  `json:"stateful"`
	Deployment           *deploymentJSON       `json:"deployment"`
	Regions              []regionSummaryJSON   `json:"regions"`
	Replicas             []topologyReplicaJSON `json:"replicas"`
}

type deploymentJSON struct {
	ID            uuid.UUID `json:"id"`
	Version       int32     `json:"version"`
	Status        string    `json:"status"`
	ImageRef      string    `json:"image_ref"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     *string   `json:"created_by"`
	CommitMessage *string   `json:"commit_message"`
}

type regionSummaryJSON struct {
	Region   string `json:"region"`
	Desired  int32  `json:"desired"`
	Observed int    `json:"observed"`
	Healthy  int    `json:"healthy"`
}

// topologyReplicaJSON is a replica as the tree shows it: joined with its host
// and deployment so the UI needs no second lookup.
type topologyReplicaJSON struct {
	ID                   uuid.UUID  `json:"id"`
	Region               string     `json:"region"`
	Hostname             *string    `json:"hostname"`
	HostID               *uuid.UUID `json:"host_id"`
	Phase                string     `json:"phase"`
	Healthy              bool       `json:"healthy"`
	DesiredStatus        string     `json:"desired_status"`
	RestartCount         int32      `json:"restart_count"`
	LastExitReason       *string    `json:"last_exit_reason"`
	UpdatedAt            time.Time  `json:"updated_at"`
	DeploymentVersion    int32      `json:"deployment_version"`
	IsCurrent            bool       `json:"is_current"`
	EnvironmentServiceID uuid.UUID  `json:"environment_service_id"`
	DeploymentID         uuid.UUID  `json:"deployment_id"`
}

// servedJSON is one (service, region) traffic pointer: which deployment version
// the region is actually serving.
type servedJSON struct {
	EnvironmentServiceID uuid.UUID `json:"environment_service_id"`
	Region               string    `json:"region"`
	DeploymentID         uuid.UUID `json:"deployment_id"`
	DeploymentVersion    int32     `json:"deployment_version"`
	Service              string    `json:"service"`
	Environment          string    `json:"environment"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func (o *OperatorAPI) meta(w http.ResponseWriter, r *http.Request) {
	meta, err := o.readMeta(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (o *OperatorAPI) readMeta(ctx context.Context) (metaJSON, error) {
	projects, err := o.store.ListProjectNames(ctx, "")
	if err != nil {
		return metaJSON{}, err
	}
	environments, err := o.store.ListEnvironmentRows(ctx, "", "")
	if err != nil {
		return metaJSON{}, err
	}
	regions, err := o.store.ListRegionNames(ctx)
	if err != nil {
		return metaJSON{}, err
	}
	return metaJSON{
		Projects:     projectsJSON(projects),
		Environments: environmentsJSON(environments),
		Regions:      orEmpty(regions),
	}, nil
}

func (o *OperatorAPI) topology(w http.ResponseWriter, r *http.Request) {
	filter := storage.TopologyFilter{
		Project:     filterValue(r, "project"),
		Environment: filterValue(r, "environment"),
		Region:      filterValue(r, "region"),
	}
	topo, err := o.readTopology(r.Context(), filter)
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, topo)
}

// readTopology assembles the whole dashboard payload from the seven slices the
// filter selects. The joins stay flat in SQL and the tree is built here: the
// alternative (nested jsonb aggregation) buys nothing and costs readability.
func (o *OperatorAPI) readTopology(ctx context.Context, filter storage.TopologyFilter) (topologyJSON, error) {
	projects, err := o.store.ListProjectNames(ctx, filter.Project)
	if err != nil {
		return topologyJSON{}, err
	}
	environments, err := o.store.ListEnvironmentRows(ctx, filter.Project, filter.Environment)
	if err != nil {
		return topologyJSON{}, err
	}
	services, err := o.store.TopologyServices(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}
	desired, err := o.store.TopologyDesiredRegions(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}
	replicas, err := o.store.TopologyReplicas(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}
	hosts, err := o.store.TopologyHosts(ctx, filter.Region)
	if err != nil {
		return topologyJSON{}, err
	}
	hostReplicas, err := o.store.ListHostReplicaCounts(ctx)
	if err != nil {
		return topologyJSON{}, err
	}
	served, err := o.store.TopologyServed(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}

	return topologyJSON{
		Tree:   buildTree(projects, environments, buildServiceNodes(services, desired, replicas)),
		Hosts:  hostsJSON(hosts, hostReplicas),
		Served: servedRowsJSON(served),
	}, nil
}

func buildServiceNodes(
	services []db.TopologyServicesRow,
	desired []db.TopologyDesiredRegionsRow,
	replicas []db.TopologyReplicasRow,
) []serviceNode {
	replicasByService := make(map[uuid.UUID][]topologyReplicaJSON, len(services))
	for _, rep := range replicas {
		replicasByService[rep.EsID] = append(replicasByService[rep.EsID], topologyReplicaJSON{
			ID: rep.ID, Region: rep.Region, Hostname: nullStr(rep.Hostname),
			HostID: nullUUID(rep.HostID), Phase: rep.Phase, Healthy: rep.Healthy,
			DesiredStatus: rep.DesiredStatus, RestartCount: rep.RestartCount,
			LastExitReason: nullStr(rep.LastExitReason), UpdatedAt: rep.UpdatedAt,
			DeploymentVersion: rep.DepVersion, IsCurrent: rep.IsCurrent,
			EnvironmentServiceID: rep.EsID, DeploymentID: rep.DeploymentID,
		})
	}

	desiredByService := make(map[uuid.UUID]map[string]int32, len(services))
	for _, row := range desired {
		if desiredByService[row.EsID] == nil {
			desiredByService[row.EsID] = map[string]int32{}
		}
		desiredByService[row.EsID][row.Region] = row.Desired
	}

	nodes := make([]serviceNode, len(services))
	for i, svc := range services {
		reps := replicasByService[svc.EsID]
		nodes[i] = serviceNode{
			EnvironmentServiceID: svc.EsID,
			EnvironmentID:        svc.EnvironmentID,
			Service:              svc.Service,
			Stateful:             svc.Stateful,
			Deployment:           currentDeployment(svc),
			Regions:              regionSummaries(desiredByService[svc.EsID], reps),
			Replicas:             orEmpty(reps),
		}
	}
	return nodes
}

func currentDeployment(svc db.TopologyServicesRow) *deploymentJSON {
	if !svc.DeploymentID.Valid {
		return nil
	}
	return &deploymentJSON{
		ID:            svc.DeploymentID.UUID,
		Version:       svc.Version.Int32,
		Status:        svc.Status.String,
		ImageRef:      svc.ImageRef.String,
		CreatedAt:     svc.CreatedAt.Time,
		CreatedBy:     nullStr(svc.CreatedBy),
		CommitMessage: nullStr(svc.CommitMessage),
	}
}

// regionSummaries reports desired-vs-observed per region. A current replica
// surviving in a region the current deployment no longer targets (post
// scale-down, mid-reap) still earns a row, so the operator can watch it go.
func regionSummaries(desired map[string]int32, reps []topologyReplicaJSON) []regionSummaryJSON {
	byRegion := make(map[string]*regionSummaryJSON, len(desired))
	for region, n := range desired {
		byRegion[region] = &regionSummaryJSON{Region: region, Desired: n}
	}
	for _, rep := range reps {
		if !rep.IsCurrent {
			continue
		}
		summary := byRegion[rep.Region]
		if summary == nil {
			summary = &regionSummaryJSON{Region: rep.Region}
			byRegion[rep.Region] = summary
		}
		if rep.DesiredStatus == "running" {
			summary.Observed++
		}
		if rep.Healthy {
			summary.Healthy++
		}
	}

	out := make([]regionSummaryJSON, 0, len(byRegion))
	for _, summary := range byRegion {
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Region < out[j].Region })
	return out
}

func buildTree(projects []string, environments []db.ListEnvironmentRowsRow, nodes []serviceNode) []projectNode {
	servicesByEnv := make(map[uuid.UUID][]serviceNode, len(environments))
	for _, node := range nodes {
		servicesByEnv[node.EnvironmentID] = append(servicesByEnv[node.EnvironmentID], node)
	}

	envsByProject := make(map[string][]environmentNode, len(projects))
	for _, e := range environments {
		envsByProject[e.ProjectName] = append(envsByProject[e.ProjectName], environmentNode{
			ID: e.ID, Name: e.Name, Services: orEmpty(servicesByEnv[e.ID]),
		})
	}

	tree := make([]projectNode, len(projects))
	for i, name := range projects {
		tree[i] = projectNode{Project: name, Environments: orEmpty(envsByProject[name])}
	}
	return tree
}

func projectsJSON(names []string) []projectJSON {
	out := make([]projectJSON, len(names))
	for i, name := range names {
		out[i] = projectJSON{Name: name}
	}
	return out
}

func environmentsJSON(rows []db.ListEnvironmentRowsRow) []environmentJSON {
	out := make([]environmentJSON, len(rows))
	for i, row := range rows {
		out[i] = environmentJSON{ID: row.ID, ProjectName: row.ProjectName, Name: row.Name}
	}
	return out
}

func servedRowsJSON(rows []db.TopologyServedRow) []servedJSON {
	out := make([]servedJSON, len(rows))
	for i, row := range rows {
		out[i] = servedJSON{
			EnvironmentServiceID: row.EnvironmentServiceID, Region: row.Region,
			DeploymentID: row.DeploymentID, DeploymentVersion: row.DepVersion,
			Service: row.Service, Environment: row.Environment, UpdatedAt: row.UpdatedAt,
		}
	}
	return out
}
