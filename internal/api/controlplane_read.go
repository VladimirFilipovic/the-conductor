package api

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"time"

	"conductor/internal/storage"
	"conductor/internal/storage/db"

	"github.com/google/uuid"
)

// Fleet reads for the operator UI. The wire shapes below are the contract the
// dashboard renders; nullable SQL columns become explicit JSON nulls so the
// client never has to know about database/sql envelope types.

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

type serviceJSON struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Stateful bool      `json:"stateful"`
}

type topologyJSON struct {
	Tree   []projectNode `json:"tree"`
	Hosts  []hostJSON    `json:"hosts"`
	Served []servedJSON  `json:"served"`
}

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
	EsID          uuid.UUID             `json:"es_id"`
	EnvironmentID uuid.UUID             `json:"environment_id"`
	Service       string                `json:"service"`
	Stateful      bool                  `json:"stateful"`
	Deployment    *deploymentJSON       `json:"deployment"`
	Regions       []regionSummaryJSON   `json:"regions"`
	Replicas      []topologyReplicaJSON `json:"replicas"`
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

type topologyReplicaJSON struct {
	ID             uuid.UUID  `json:"id"`
	Region         string     `json:"region"`
	Hostname       *string    `json:"hostname"`
	HostID         *uuid.UUID `json:"host_id"`
	Phase          string     `json:"phase"`
	Healthy        bool       `json:"healthy"`
	DesiredStatus  string     `json:"desired_status"`
	RestartCount   int32      `json:"restart_count"`
	LastExitReason *string    `json:"last_exit_reason"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DepVersion     int32      `json:"dep_version"`
	IsCurrent      bool       `json:"is_current"`
	EsID           uuid.UUID  `json:"es_id"`
	DeploymentID   uuid.UUID  `json:"deployment_id"`
}

type servedJSON struct {
	EnvironmentServiceID uuid.UUID `json:"environment_service_id"`
	Region               string    `json:"region"`
	DeploymentID         uuid.UUID `json:"deployment_id"`
	DepVersion           int32     `json:"dep_version"`
	Service              string    `json:"service"`
	Environment          string    `json:"environment"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func (c *ControlPlane) meta(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := c.store.ListProjectNames(ctx, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	environments, err := c.store.ListEnvironmentRows(ctx, "", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	regions, err := c.store.ListRegionNames(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, metaJSON{
		Projects:     projectsJSON(projects),
		Environments: environmentsJSON(environments),
		Regions:      orEmpty(regions),
	})
}

// listServices backs the bind form: project-wide by default so a service can be
// bound into an environment it is not in yet; ?environment= narrows to the
// services already bound there.
func (c *ControlPlane) listServices(w http.ResponseWriter, r *http.Request) {
	project := filterValue(r, "project")
	if project == "" {
		writeError(w, http.StatusBadRequest, errors.New("project is required"))
		return
	}
	rows, err := c.store.ListProjectServices(r.Context(), project, filterValue(r, "environment"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]serviceJSON, len(rows))
	for i, row := range rows {
		out[i] = serviceJSON{ID: row.ID, Name: row.Name, Stateful: row.Stateful}
	}
	writeJSON(w, http.StatusOK, map[string][]serviceJSON{"services": out})
}

// deploymentReplicas lists the fan-out targets for deployment-wide agent chaos.
func (c *ControlPlane) deploymentReplicas(w http.ResponseWriter, r *http.Request) {
	deploymentID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad deployment id"))
		return
	}
	ids, err := c.store.ListDeploymentReplicaIDs(r.Context(), deploymentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]uuid.UUID{"replica_ids": orEmpty(ids)})
}

func (c *ControlPlane) topology(w http.ResponseWriter, r *http.Request) {
	filter := storage.TopologyFilter{
		Project:     filterValue(r, "project"),
		Environment: filterValue(r, "environment"),
		Region:      filterValue(r, "region"),
	}
	topo, err := c.readTopology(r, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, topo)
}

// readTopology assembles the whole dashboard payload from the seven slices the
// filter selects. The joins stay flat in SQL and the tree is built here: the
// alternative (nested jsonb aggregation) buys nothing and costs readability.
func (c *ControlPlane) readTopology(r *http.Request, filter storage.TopologyFilter) (topologyJSON, error) {
	ctx := r.Context()
	projects, err := c.store.ListProjectNames(ctx, filter.Project)
	if err != nil {
		return topologyJSON{}, err
	}
	environments, err := c.store.ListEnvironmentRows(ctx, filter.Project, filter.Environment)
	if err != nil {
		return topologyJSON{}, err
	}
	services, err := c.store.TopologyServices(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}
	desired, err := c.store.TopologyDesiredRegions(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}
	replicas, err := c.store.TopologyReplicas(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}
	hosts, err := c.store.TopologyHosts(ctx, filter.Region)
	if err != nil {
		return topologyJSON{}, err
	}
	served, err := c.store.TopologyServed(ctx, filter)
	if err != nil {
		return topologyJSON{}, err
	}

	return topologyJSON{
		Tree:   buildTree(projects, environments, buildServiceNodes(services, desired, replicas)),
		Hosts:  hostsJSON(hosts),
		Served: servedRowsJSON(served),
	}, nil
}

func buildServiceNodes(
	services []db.TopologyServicesRow,
	desired []db.TopologyDesiredRegionsRow,
	replicas []db.TopologyReplicasRow,
) []serviceNode {
	replicasByEs := make(map[uuid.UUID][]topologyReplicaJSON, len(services))
	for _, rep := range replicas {
		replicasByEs[rep.EsID] = append(replicasByEs[rep.EsID], topologyReplicaJSON{
			ID: rep.ID, Region: rep.Region, Hostname: nullStr(rep.Hostname),
			HostID: nullUUID(rep.HostID), Phase: rep.Phase, Healthy: rep.Healthy,
			DesiredStatus: rep.DesiredStatus, RestartCount: rep.RestartCount,
			LastExitReason: nullStr(rep.LastExitReason), UpdatedAt: rep.UpdatedAt,
			DepVersion: rep.DepVersion, IsCurrent: rep.IsCurrent,
			EsID: rep.EsID, DeploymentID: rep.DeploymentID,
		})
	}

	desiredByEs := make(map[uuid.UUID]map[string]int32, len(services))
	for _, row := range desired {
		if desiredByEs[row.EsID] == nil {
			desiredByEs[row.EsID] = map[string]int32{}
		}
		desiredByEs[row.EsID][row.Region] = row.Desired
	}

	nodes := make([]serviceNode, len(services))
	for i, svc := range services {
		reps := replicasByEs[svc.EsID]
		nodes[i] = serviceNode{
			EsID:          svc.EsID,
			EnvironmentID: svc.EnvironmentID,
			Service:       svc.Service,
			Stateful:      svc.Stateful,
			Deployment:    currentDeployment(svc),
			Regions:       regionSummaries(desiredByEs[svc.EsID], reps),
			Replicas:      orEmpty(reps),
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
	regions := make(map[string]struct{}, len(desired))
	for region := range desired {
		regions[region] = struct{}{}
	}
	for _, rep := range reps {
		if rep.IsCurrent {
			regions[rep.Region] = struct{}{}
		}
	}

	names := make([]string, 0, len(regions))
	for region := range regions {
		names = append(names, region)
	}
	sort.Strings(names)

	out := make([]regionSummaryJSON, len(names))
	for i, region := range names {
		summary := regionSummaryJSON{Region: region, Desired: desired[region]}
		for _, rep := range reps {
			if !rep.IsCurrent || rep.Region != region {
				continue
			}
			if rep.DesiredStatus == "running" {
				summary.Observed++
			}
			if rep.Healthy {
				summary.Healthy++
			}
		}
		out[i] = summary
	}
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
			DeploymentID: row.DeploymentID, DepVersion: row.DepVersion,
			Service: row.Service, Environment: row.Environment, UpdatedAt: row.UpdatedAt,
		}
	}
	return out
}

// filterValue reads a selector param. "all" is the UI's own word for "no
// filter" and is treated as absent, so a stale query string can't blank a view.
func filterValue(r *http.Request, name string) string {
	v := r.URL.Query().Get(name)
	if v == "all" {
		return ""
	}
	return v
}

func nullStr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// orEmpty keeps a nil slice out of the JSON: clients iterate these unguarded,
// and `null` is not iterable.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
