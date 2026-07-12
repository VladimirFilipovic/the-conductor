package builder_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"conductor/builder"
)

// stubBuilder returns a fixed ref (or error) and records that it ran, so a test
// can assert whether Resolve actually invoked the build path.
type stubBuilder struct {
	ref    string
	err    error
	called bool
}

func (s *stubBuilder) Build(_ context.Context, _ builder.Request) (builder.Result, error) {
	s.called = true
	return builder.Result{ImageRef: s.ref}, s.err
}

func TestResolve_PrebuiltImageSkipsBuild(t *testing.T) {
	b := &stubBuilder{ref: "should-not-be-used"}
	got, err := builder.Resolve(context.Background(), b, builder.Request{Service: "web", Repo: "github.com/acme/web"}, "ghcr.io/acme/web:1.2.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ghcr.io/acme/web:1.2.3" {
		t.Errorf("got %q, want the prebuilt image", got)
	}
	if b.called {
		t.Error("builder ran even though a prebuilt image was given")
	}
}

func TestResolve_RepoBuilds(t *testing.T) {
	b := &stubBuilder{ref: "conductor.local/web:source"}
	got, err := builder.Resolve(context.Background(), b, builder.Request{Service: "web", Repo: "github.com/acme/web"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !b.called {
		t.Error("builder did not run for a repo service")
	}
	if got != "conductor.local/web:source" {
		t.Errorf("got %q, want the built ref", got)
	}
}

func TestResolve_NoSource(t *testing.T) {
	b := &stubBuilder{ref: "x"}
	_, err := builder.Resolve(context.Background(), b, builder.Request{Service: "web"}, "")
	if err == nil || !strings.Contains(err.Error(), "no source") {
		t.Fatalf("want 'no source' error, got %v", err)
	}
	if b.called {
		t.Error("builder ran for a service with no source")
	}
}

func TestResolve_EmptyRefIsError(t *testing.T) {
	b := &stubBuilder{ref: ""} // builder yields nothing
	_, err := builder.Resolve(context.Background(), b, builder.Request{Service: "web", Repo: "github.com/acme/web"}, "")
	if err == nil || !strings.Contains(err.Error(), "empty image ref") {
		t.Fatalf("want 'empty image ref' error, got %v", err)
	}
}

func TestResolve_BuildErrorPropagates(t *testing.T) {
	b := &stubBuilder{err: errors.New("boom")}
	_, err := builder.Resolve(context.Background(), b, builder.Request{Service: "web", Repo: "github.com/acme/web"}, "")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped build error, got %v", err)
	}
}
