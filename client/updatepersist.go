package client

// Client-side wiring between the update engine, the session persistence,
// and the connect lifecycle (Phase 10).

import (
	"time"

	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl/gen"
)

// persistInterval rate-limits session saves from the update loop (the
// reference defers SQLite persistence to its keepalive loop;
// updates.py:298, 521-531 — never persist per update).
const persistInterval = time.Second

// persistUpdateState stores the MessageBox state into the session and
// saves UNCONDITIONALLY (rate limiting lives in the update manager's
// persistNow, which must not consume the dirty flag when it skips the
// save — Phase 15 bug: the rate limiter swallowed the shutdown flush).
func (c *Client) persistUpdateState(ss mbSessionState, cs []mbChannelState) {
	_ = c.sess.SetUpdateState(session.UpdateStateRecord{
		Pts: ss.Pts, Qts: ss.Qts, Date: ss.Date, Seq: ss.Seq,
	})
	for _, ch := range cs {
		_ = c.sess.SetChannelUpdateState(ch.ChannelID, ch.Pts)
	}
	if c.cfg.Saver != nil {
		_ = c.cfg.Saver.Save()
	}
}

// seedUpdateStateFromSession loads the persisted update state into the
// MessageBox at connect (the reference's catch_up seeding path,
// telegrambaseclient.py:578-591 + MessageBox.load).
func (c *Client) seedUpdateStateFromSession() {
	rec, ok := c.sess.UpdateState()
	if !ok {
		return
	}
	records := c.sess.ChannelUpdateStates()
	chans := make([]mbChannelState, len(records))
	for i, ch := range records {
		chans[i] = mbChannelState{ChannelID: ch.ChannelID, Pts: ch.Pts}
	}
	c.mu.RLock()
	m := c.updates
	c.mu.RUnlock()
	if m != nil {
		m.box.load(mbSessionState{Pts: rec.Pts, Qts: rec.Qts, Date: rec.Date, Seq: rec.Seq}, chans)
	}
}

// CatchUp runs a full account difference now — the reference's
// catch_up() (updates.py:248-262), which queues a synthetic
// updatesTooLong to trigger recovery through the normal pipeline.
func (c *Client) CatchUp() error {
	c.mu.RLock()
	m := c.updates
	c.mu.RUnlock()
	if m == nil || c.closed.Load() {
		return ErrNotReady
	}
	select {
	case m.rawQ <- &gen.UpdatesTooLong{}:
	default:
		// Raw queue full: the deadline machinery re-triggers recovery
		// (bounded, never blocks the caller).
	}
	return nil
}
