package engine

import (
	"conductor/internal/storage"
)

type Orchestrator struct {
	store storage.Store
}

type Sensor struct {
	store storage.Store
}

func Run(orchestrator *Orchestrator, sensor *Sensor) error {
	return nil
}
