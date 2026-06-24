package domain

// DeploymentStatus is the lifecycle of an append-only deployment commit (§13).
// String-underlying to map onto the text column; keep in lockstep with the CHECK
// on deployments.status. Orthogonal to deployments.is_current — is_current marks
// the one active commit per service env, status tracks how this commit is faring.
type DeploymentStatus string

const (
	DeploymentPending    DeploymentStatus = "pending"    // committed, rollout not started
	DeploymentActive     DeploymentStatus = "active"     // current commit, all target replicas healthy
	DeploymentDraining   DeploymentStatus = "draining"   // rollout in flight: new up, old draining
	DeploymentFailed     DeploymentStatus = "failed"     // new replicas crashlooped; rollout frozen
	DeploymentRolledBack DeploymentStatus = "rolledback" // reverted to a prior version
	DeploymentSuperseded DeploymentStatus = "superseded" // replaced by a newer commit
)

// Valid reports whether s is a known status — guard before writing one back.
func (s DeploymentStatus) Valid() bool {
	switch s {
	case DeploymentPending, DeploymentActive, DeploymentDraining,
		DeploymentFailed, DeploymentRolledBack, DeploymentSuperseded:
		return true
	}
	return false
}
