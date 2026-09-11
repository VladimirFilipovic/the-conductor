package domain

import "time"

// VolumeLeaseTTL bounds how long a dead writer can hold a volume hostage. It is
// shared by the two sides of the lease: the engine acquires with it on
// placement, the API's ingest renews with it on every healthy observation that
// lands — so a lease only outlives its holder by one TTL, never longer.
const VolumeLeaseTTL = 90 * time.Second

// GatewayLivenessWindow is how long an apiserver may go without refreshing its
// gateway_instances heartbeat before the engine stops counting it as live. The
// apiserver refreshes far more often than this; the window only has to absorb
// a stalled tick or a slow database, never a real outage.
const GatewayLivenessWindow = 30 * time.Second
