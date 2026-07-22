package status

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"text/tabwriter"

	"conductor/internal/storage/db"
	"conductor/internal/target"
)

// Store is the read side this package needs. Existence checks run first so an
// unknown project/environment/service surfaces as storage.ErrNotFound rather
// than an empty table that misreads as "exists but nothing deployed".
type Store interface {
	GetProject(ctx context.Context, name string) (db.Project, error)
	GetEnvironment(ctx context.Context, projectName, name string) (db.Environment, error)
	GetService(ctx context.Context, projectName, name string) (db.Service, error)
	ProjectStatus(ctx context.Context, projectName, environment, service string) ([]db.ProjectStatusRow, error)
}

type Service struct {
	store Store
}

func New(store Store) *Service {
	return &Service{store: store}
}

// Fetch returns the project's observed-vs-desired rows, optionally narrowed by
// environment/service. Each named scope is existence-checked first, so a typo'd
// -e reports "not found" instead of the misleading "no services match".
func (s *Service) Fetch(ctx context.Context, t target.Target) ([]db.ProjectStatusRow, error) {
	if _, err := s.store.GetProject(ctx, t.Project); err != nil {
		return nil, err
	}
	if t.Environment != "" {
		if _, err := s.store.GetEnvironment(ctx, t.Project, t.Environment); err != nil {
			return nil, err
		}
	}
	if t.Service != "" {
		if _, err := s.store.GetService(ctx, t.Project, t.Service); err != nil {
			return nil, err
		}
	}
	return s.store.ProjectStatus(ctx, t.Project, t.Environment, t.Service)
}

// Render prints one aligned section per environment. Rows arrive ordered by
// (environment, service), so a fresh tabwriter is flushed at each environment
// boundary — every section's columns size to their own content.
func Render(w io.Writer, projectName string, rows []db.ProjectStatusRow) {
	fmt.Fprintf(w, "project %s\n", projectName)
	for i := 0; i < len(rows); {
		env := rows[i].Environment
		fmt.Fprintf(w, "\n%s\n", env)
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  SERVICE\tKIND\tDEPLOY\tSTATUS\tDESIRED\tHEALTHY")
		for ; i < len(rows) && rows[i].Environment == env; i++ {
			r := rows[i]
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%d/%d\n",
				r.Service, kindOf(r.Stateful), deployLabel(r.DeployVersion),
				orDash(r.DeployStatus), r.DesiredReplicas,
				r.HealthyReplicas, r.ObservedReplicas)
		}
		_ = tw.Flush()
	}
}

// Scope describes the requested slice for the empty-result message: the project
// alone, or the environment/service it was narrowed to.
func Scope(t target.Target) string {
	switch {
	case t.Service != "":
		return fmt.Sprintf("service %q in %s/%s", t.Service, t.Project, dash(t.Environment))
	case t.Environment != "":
		return fmt.Sprintf("environment %q in project %s", t.Environment, t.Project)
	default:
		return fmt.Sprintf("project %s", t.Project)
	}
}

func kindOf(stateful bool) string {
	if stateful {
		return "stateful"
	}
	return "service"
}

func deployLabel(v sql.NullInt32) string {
	if !v.Valid {
		return "—"
	}
	return fmt.Sprintf("v%d", v.Int32)
}

func orDash(s sql.NullString) string {
	if !s.Valid || s.String == "" {
		return "—"
	}
	return s.String
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
