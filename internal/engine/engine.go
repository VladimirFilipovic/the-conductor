package engine

type Orchestrator struct {
	store OrchestratorStore
}

type Sensor struct {
	store SensorStore
}

func Run(orchestrator *Orchestrator, sensor *Sensor) error {
	return nil
}
