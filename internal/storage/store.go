package storage

// Querier is the full set of control-plane queries and the ONE interface this
// package exports. It types the tx-scoped value WithTx and WithReadTx hand to
// their callbacks — a value nothing outside storage can produce, which is the
// only reason an interface belongs here at all.
//
// The rule: storage exports one type for the tx-scoped value it hands to
// callbacks. Every consumer declares the narrow view it needs on its own side
// (see status.Store, project.Store, engine.SnapshotReader, engine.ReconcileTx)
// and narrows inside the callback by passing the Querier to a helper typed with
// that view. Nothing is added here to serve a caller; methods land here only
// because querier implements them.
//
// The embedded groups are unexported and mirror the file each slice is
// implemented in; they exist for readability, not as fences.
type Querier interface {
	projectQuerier
	deploymentQuerier
	volumeQuerier
	snapshotQuerier
	reconcileQuerier
	sensorQuerier
	controlPlaneQuerier
}

var _ Querier = querier{}
