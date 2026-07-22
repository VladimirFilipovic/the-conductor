package project_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"conductor/internal/project"
	"conductor/internal/storage"
	"conductor/internal/storage/db"
	"conductor/internal/target"

	"github.com/google/uuid"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    int32
		wantErr bool
	}{
		{"", 0, false}, // empty ⇒ "previous version"
		{"v2", 2, false},
		{"3", 3, false},
		{"  v4 ", 4, false},
		{"v0", 0, true}, // non-positive
		{"-1", 0, true},
		{"banana", 0, true},
		{"v", 0, true},
	}
	for _, tc := range tests {
		got, err := project.ParseVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q): want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q): unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseVersion(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// fakeStore implements storage.TxStore with only the methods Rollback touches;
// anything else panics via the embedded nil Store, asserting Rollback reaches for
// nothing more. WithTx runs the callback on the fake, so rollback is in-memory.
type fakeStore struct {
	storage.Store

	es        db.GetEnvironmentServiceRow
	current   db.GetCurrentDeploymentRow
	currentEr error
	prev      int32
	prevErr   error
	byVersion map[int32]db.GetDeploymentByVersionRow

	marked       bool
	setCurrentID uuid.UUID
}

func (f *fakeStore) WithTx(_ context.Context, fn func(storage.Store) error) error {
	return fn(f)
}

func (f *fakeStore) GetEnvironmentService(_ context.Context, _, _, _ string) (db.GetEnvironmentServiceRow, error) {
	return f.es, nil
}

func (f *fakeStore) GetCurrentDeployment(_ context.Context, _ uuid.UUID) (db.GetCurrentDeploymentRow, error) {
	return f.current, f.currentEr
}

func (f *fakeStore) PreviousDeploymentVersion(_ context.Context, _ uuid.UUID, _ int32) (int32, error) {
	return f.prev, f.prevErr
}

func (f *fakeStore) GetDeploymentByVersion(_ context.Context, _ uuid.UUID, version int32) (db.GetDeploymentByVersionRow, error) {
	row, ok := f.byVersion[version]
	if !ok {
		return db.GetDeploymentByVersionRow{}, errors.New("not found")
	}
	return row, nil
}

func (f *fakeStore) MarkCurrentRolledBack(_ context.Context, _ uuid.UUID) error {
	f.marked = true
	return nil
}

func (f *fakeStore) SetDeploymentCurrent(_ context.Context, id uuid.UUID) error {
	f.setCurrentID = id
	return nil
}

// newFake builds a fake whose current deployment is v3 and that knows about
// versions 1..3, so the common "roll back from v3" cases need no extra setup.
func newFake() *fakeStore {
	v1, v2, v3 := uuid.New(), uuid.New(), uuid.New()
	return &fakeStore{
		es:      db.GetEnvironmentServiceRow{ID: uuid.New()},
		current: db.GetCurrentDeploymentRow{ID: v3, Version: 3},
		byVersion: map[int32]db.GetDeploymentByVersionRow{
			1: {ID: v1, Version: 1},
			2: {ID: v2, Version: 2},
			3: {ID: v3, Version: 3},
		},
	}
}

func tgt() target.Target {
	return target.Target{Project: "p", Environment: "e", Service: "s"}
}

func TestRollback_DefaultToPrevious(t *testing.T) {
	f := newFake()
	f.prev = 2 // PreviousDeploymentVersion(before=3) → 2

	res, err := project.New(f).Rollback(context.Background(), project.RollbackInput{Target: tgt()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.From != 3 || res.To != 2 {
		t.Fatalf("got v%d→v%d, want v3→v2", res.From, res.To)
	}
	if !f.marked {
		t.Error("old current was not marked rolled back")
	}
	if f.setCurrentID != f.byVersion[2].ID {
		t.Error("did not promote the v2 row")
	}
}

func TestRollback_ToSpecificVersion(t *testing.T) {
	f := newFake()

	res, err := project.New(f).Rollback(context.Background(), project.RollbackInput{Target: tgt(), ToVersion: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.From != 3 || res.To != 1 {
		t.Fatalf("got v%d→v%d, want v3→v1", res.From, res.To)
	}
	if f.setCurrentID != f.byVersion[1].ID {
		t.Error("did not promote the v1 row")
	}
}

func TestRollback_AlreadyAtTarget(t *testing.T) {
	f := newFake()

	_, err := project.New(f).Rollback(context.Background(), project.RollbackInput{Target: tgt(), ToVersion: 3})
	if err == nil || !strings.Contains(err.Error(), "already at v3") {
		t.Fatalf("want 'already at v3' error, got %v", err)
	}
	if f.marked {
		t.Error("must not mutate when already at target")
	}
}

func TestRollback_NoSuchVersion(t *testing.T) {
	f := newFake()

	_, err := project.New(f).Rollback(context.Background(), project.RollbackInput{Target: tgt(), ToVersion: 9})
	if err == nil || !strings.Contains(err.Error(), "no such version v9") {
		t.Fatalf("want 'no such version v9' error, got %v", err)
	}
	if f.marked {
		t.Error("must not mutate when target version is missing")
	}
}

func TestRollback_NoCurrentDeployment(t *testing.T) {
	f := newFake()
	f.currentEr = errors.New("no current deployment")

	_, err := project.New(f).Rollback(context.Background(), project.RollbackInput{Target: tgt()})
	if err == nil || !strings.Contains(err.Error(), "no current deployment") {
		t.Fatalf("want no-current error, got %v", err)
	}
}

func TestRollback_NoPreviousVersion(t *testing.T) {
	f := newFake()
	f.current = db.GetCurrentDeploymentRow{ID: f.byVersion[1].ID, Version: 1}
	f.prevErr = errors.New("not found") // nothing below v1

	_, err := project.New(f).Rollback(context.Background(), project.RollbackInput{Target: tgt()})
	if err == nil || !strings.Contains(err.Error(), "no earlier version") {
		t.Fatalf("want 'no earlier version' error, got %v", err)
	}
	if f.marked {
		t.Error("must not mutate when there is no earlier version")
	}
}
