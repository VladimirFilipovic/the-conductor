package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// listenReconnectDelay paces reconnect attempts after the LISTEN connection
// drops; notifications missed in the gap are healed by onReconnect (and the
// gateway's periodic resync), so the delay costs latency, never correctness.
const listenReconnectDelay = 2 * time.Second

// listenIdleTimeout bounds a single WaitForNotification so a half-open
// connection (peer gone, no FIN — NAT expiry, killed VM) is detected by a
// Ping instead of blocking forever with LISTEN silently dead.
const listenIdleTimeout = 30 * time.Second

// ListenReplicaChanges blocks on the replicas_changed channel (fed by the
// notify_replicas_changed trigger) and invokes onChange with the affected host
// id for every notification. It holds a dedicated pgx connection — LISTEN is
// session-scoped, so it can't ride the pooled database/sql client — and
// reconnects forever until ctx ends. Payloads are keys, not data: the caller
// re-reads fresh state, so drops and duplicates are both harmless.
//
// onReconnect (nil allowed) fires once per successful LISTEN, including the
// first: anything fired while no connection was listening is gone for good,
// so the caller must treat every host as possibly changed.
func ListenReplicaChanges(ctx context.Context, dsn string, onChange func(hostID uuid.UUID), onReconnect func()) error {
	for {
		err := listenOnce(ctx, dsn, onChange, onReconnect)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("storage: replica listener reconnecting", "err", err)
		select {
		case <-time.After(listenReconnectDelay):
		case <-ctx.Done():
			return nil
		}
	}
}

func listenOnce(ctx context.Context, dsn string, onChange func(hostID uuid.UUID), onReconnect func()) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("listen connect: %w", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN replicas_changed"); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if onReconnect != nil {
		onReconnect()
	}
	for {
		n, err := waitForNotification(ctx, conn)
		if err != nil {
			return err
		}
		if n == nil {
			continue // idle timeout, connection verified alive
		}
		hostID, err := uuid.Parse(n.Payload)
		if err != nil {
			slog.Warn("storage: notification with bad payload dropped", "payload", n.Payload)
			continue
		}
		onChange(hostID)
	}
}

// waitForNotification is one bounded WaitForNotification. A nil, nil return
// means the idle timeout hit and Ping proved the connection alive. The idle
// deadline is told apart from a parent cancellation by the parent's own Err,
// not by inspecting pgx's error — pgx wraps timeouts in a type without Unwrap.
func waitForNotification(ctx context.Context, conn *pgx.Conn) (*pgconn.Notification, error) {
	waitCtx, cancel := context.WithTimeout(ctx, listenIdleTimeout)
	defer cancel()
	n, err := conn.WaitForNotification(waitCtx)
	if err == nil {
		return n, nil
	}
	if ctx.Err() != nil || waitCtx.Err() != context.DeadlineExceeded {
		return nil, fmt.Errorf("wait notification: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("listen ping: %w", err)
	}
	return nil, nil
}
