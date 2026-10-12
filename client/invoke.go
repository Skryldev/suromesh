package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// SetFloodWaitThreshold overrides the flood-wait tolerance for this
// client (requests sleeping through waits ≤ threshold and retrying).
func (c *Client) SetFloodWaitThreshold(d time.Duration) {
	c.mu.Lock()
	c.cfg.FloodWaitThreshold = d
	c.mu.Unlock()
}

// Invoke executes one RPC request and returns its decoded result.
//
// Retry semantics (reference parity, users.py:_call):
//   - ServerError/RpcCallFailError-class: retried (server internal issue,
//     request provably not executed).
//   - FloodWait ≤ FloodWaitThreshold: slept through, then retried (the
//     server did not execute the request).
//   - FloodWait above the threshold: returned to the caller.
//   - Migration errors (PHONE_MIGRATE_X etc.): returned — DC migration is
//     a Phase 6+ concern once multi-DC transport exists (Soroush routes
//     all DCs through one endpoint today).
//   - Connection loss mid-flight: NEVER retried (the request may have
//     executed; replaying risks duplicate side effects).
//
// Context cancellation always wins over retry loops.
func (c *Client) Invoke(ctx context.Context, req tl.Request) (tl.Object, error) {
	// Fail fast when the client cannot serve RPCs (preserves the original
	// error precedence: engine/lifecycle errors before input validation).
	if _, err := c.engine(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("client: nil request")
	}
	method := methodName(req)
	if c.cfg.Hooks.OnRPCStart != nil {
		c.cfg.Hooks.OnRPCStart(method)
	}
	// Splice the lifecycle context: client Close interrupts in-flight
	// requests and retry backoff sleeps deterministically.
	opCtx, cancelOp := mergeContext(ctx, c.lifecycleCtx)
	defer cancelOp()

	start := time.Now()

	res, err := c.invokeWithRetries(opCtx, req, 0)

	if c.cfg.Hooks.OnRPCComplete != nil {
		c.cfg.Hooks.OnRPCComplete(method, time.Since(start), err)
	}
	if err != nil {
		// A lifecycle interruption during retry backoff must surface as
		// ErrClosed (the client is gone), not a bare context error.
		if opCtx.Err() != nil && c.closed.Load() {
			return nil, ErrClosed
		}
		// 401-class failures reflect authorization, not transport: drop the
		// durable authorized flag (and mark permanent revocations) so the
		// client's auth state tracks the server, wherever the rejection
		// surfaces (reference: UnauthorizedError clears _authorized).
		c.handleAuthReject(err)
		return nil, err
	}
	c.extractEntities(res)
	return res, nil
}

func (c *Client) invokeWithRetries(ctx context.Context, req tl.Request, attempt int) (tl.Object, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	res, err := eng.Invoke(ctx, req)
	if err == nil {
		return res, nil
	}

	// Context cancellation/deadline always wins.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctx.Err()
	}

	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		return nil, err // non-RPC failures are not retried
	}

	switch {
	case isServerInternalError(rpcErr):
		if attempt >= c.cfg.Retries {
			return nil, err
		}
		return c.retryAfter(ctx, req, attempt, 2*time.Second, err)

	case rpcErr.IsFloodWait():
		wait, _ := rpcErr.RetryAfter()
		if wait > c.cfg.FloodWaitThreshold {
			return nil, err // surface long waits to the caller
		}
		if attempt >= c.cfg.Retries {
			return nil, err
		}
		return c.retryAfter(ctx, req, attempt, wait, err)

	default:
		return nil, err
	}
}

func (c *Client) retryAfter(ctx context.Context, req tl.Request, attempt int, d time.Duration, cause error) (tl.Object, error) {
	if c.cfg.Hooks.OnRPCRetry != nil {
		c.cfg.Hooks.OnRPCRetry(methodName(req), attempt+1, cause)
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.invokeWithRetries(ctx, req, attempt+1)
}

// isServerInternalError matches the reference's retry set: 500-class
// server errors and RPC_CALL_FAIL / RPC_MCGET_FAIL.
func isServerInternalError(e *rpc.RPCError) bool {
	if e.Code >= 500 {
		return true
	}
	switch e.Type {
	case "RPC_CALL_FAIL", "RPC_MCGET_FAIL", "INTERDC_CALL_ERROR", "INTERDC_CALL_RICH_ERROR", "TIMED_OUT":
		return true
	}
	return false
}

// ---- resolution --------------------------------------------------------

// ResolveUsername resolves a username to a domain entity, caching the
// access hashes learned from the response.
func (c *Client) ResolveUsername(ctx context.Context, username string) (Peer, error) {
	if username == "" {
		return nil, errors.New("client: empty username")
	}
	if username[0] == '@' {
		username = username[1:]
	}
	res, err := c.Invoke(ctx, &gen.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, err
	}
	resolved, ok := res.(*gen.ContactsResolvedPeer)
	if !ok {
		return nil, fmt.Errorf("client: resolveUsername answered %T", res)
	}
	// Seed already done by Invoke; pick the entity from the cache.
	switch p := resolved.Peer.(type) {
	case *gen.PeerUser:
		if ent, found := c.entities.get(PeerID{PeerUser, p.UserID}); found {
			if u, ok := ent.(*User); ok {
				return u, nil
			}
		}
		return nil, &ErrAccessHashUnavailable{Peer: PeerID{PeerUser, p.UserID}}
	case *gen.PeerChannel:
		if ent, found := c.entities.get(PeerID{PeerChannel, p.ChannelID}); found {
			if ch, ok := ent.(*Channel); ok {
				return ch, nil
			}
		}
		return nil, &ErrAccessHashUnavailable{Peer: PeerID{PeerChannel, p.ChannelID}}
	case *gen.PeerChat:
		if ent, found := c.entities.get(PeerID{PeerChat, p.ChatID}); found {
			if ch, ok := ent.(*Chat); ok {
				return ch, nil
			}
		}
		return nil, &ErrAccessHashUnavailable{Peer: PeerID{PeerChat, p.ChatID}}
	}
	return nil, errors.New("client: resolveUsername: empty peer")
}

// InputPeerFor turns any supported peer into the wire InputPeer,
// resolving via the entity cache (never fabricating access hashes).
// Migration note: Phase 6's ResolvePeer(Peer) → (InputPeerClass) was
// renamed in Phase 11 so that ResolvePeer can carry the reference's
// entity-resolution semantics (PeerRef → domain Peer); wire conversion
// lives here.
func (c *Client) InputPeerFor(ctx context.Context, p Peer) (gen.InputPeerClass, error) {
	return c.inputPeer(p)
}
