package status

import (
	"database/sql"
	"strings"
	"testing"

	"conductor/internal/storage/db"
)

// Degraded is a read-side derivation over an active deployment: only the
// healthy-below-desired case earns the suffix, every other status prints as
// stored.
func TestStatusLabelDerivesDegraded(t *testing.T) {
	active := sql.NullString{String: "active", Valid: true}
	tests := []struct {
		name string
		row  db.ProjectStatusRow
		want string
	}{
		{"converged", db.ProjectStatusRow{DeployStatus: active, DesiredReplicas: 2, HealthyReplicas: 2}, "active"},
		{"frozen replica", db.ProjectStatusRow{DeployStatus: active, DesiredReplicas: 2, HealthyReplicas: 1}, "active (degraded)"},
		{"scaled to zero", db.ProjectStatusRow{DeployStatus: active, DesiredReplicas: 0, HealthyReplicas: 0}, "active"},
		{"failed rollout keeps its word", db.ProjectStatusRow{DeployStatus: sql.NullString{String: "failed", Valid: true}, DesiredReplicas: 2, HealthyReplicas: 1}, "failed"},
		{"never deployed", db.ProjectStatusRow{DesiredReplicas: 0}, "—"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusLabel(tt.row); got != tt.want {
				t.Errorf("statusLabel = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderShowsDegradedColumn(t *testing.T) {
	var out strings.Builder
	Render(&out, "demo", []db.ProjectStatusRow{{
		Environment: "production", Service: "web",
		DeployVersion:   sql.NullInt32{Int32: 3, Valid: true},
		DeployStatus:    sql.NullString{String: "active", Valid: true},
		DesiredReplicas: 2, HealthyReplicas: 1, ObservedReplicas: 2,
	}})
	if !strings.Contains(out.String(), "active (degraded)") || !strings.Contains(out.String(), "1/2") {
		t.Fatalf("render lacks the degraded label or the healthy/observed count:\n%s", out.String())
	}
}
