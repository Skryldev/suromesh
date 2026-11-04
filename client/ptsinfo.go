package client

// ptsInfo extracts the MessageBox ordering key from one protocol update —
// the port of PtsInfo.from_update (messagebox.py:89-123):
//
//	pts   → (pts, pts_count, entry)   entry = channel_id, or
//	        update.message.peer_id.channel_id, else ACCOUNT
//	qts   → (qts, 1, SECRET)
//	neither → no ordering info (order-independent application)
//
// The switch enumerates EVERY pts/qts-carrying update constructor in
// layer 182 (from tl/schema/api.tl); any constructor not listed falls
// through to "no pts" — which per the reference applies in any order and
// is never silently dropped.

import (
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func ptsInfo(u tl.Object) (ptsUpdate, bool) {
	pi := ptsUpdate{obj: u, entry: mbAccount}
	defer func() {}()
	out, ok := ptsInfoInner(u, pi)
	// Python truthiness (messagebox.py:106 `if pts:` / 116 `if qts:`): a
	// ZERO pts/qts means "no ordering info" — the reference relies on this
	// so synthetic difference updates (pts=0) apply unconditionally.
	if ok && out.pts == 0 {
		return pi, false
	}
	return out, ok
}

func ptsInfoInner(u tl.Object, pi ptsUpdate) (ptsUpdate, bool) {
	switch t := u.(type) {
	// ---- pts-carrying, account scope (or message-scoped) -------------
	case *gen.UpdateNewMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromMessagePeer(t.Message)
		return pi, true
	case *gen.UpdateEditMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromMessagePeer(t.Message)
		return pi, true
	case *gen.UpdateDeleteMessages:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	case *gen.UpdateReadHistoryInbox:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromPeer(t.Peer)
		return pi, true
	case *gen.UpdateReadHistoryOutbox:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromPeer(t.Peer)
		return pi, true
	case *gen.UpdateWebPage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	case *gen.UpdateReadMessagesContents:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	case *gen.UpdateFolderPeers:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	case *gen.UpdatePinnedMessages:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromPeer(t.Peer)
		return pi, true

	// ---- pts-carrying, channel scope ---------------------------------
	case *gen.UpdateNewChannelMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromMessagePeer(t.Message)
		if pi.entry == mbAccount {
			// Fallback like the reference's channel_id attribute: a
			// channel message without a parseable peer keeps ACCOUNT
			// (messagebox.py:108-114).
			pi.entry = mbAccount
		}
		pi.channel = pi.entry > 0
		return pi, true
	case *gen.UpdateEditChannelMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry = entryFromMessagePeer(t.Message)
		pi.channel = pi.entry > 0
		return pi, true
	case *gen.UpdateDeleteChannelMessages:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry, pi.channel = mbEntry(t.ChannelID), true
		return pi, true
	case *gen.UpdateReadChannelInbox:
		pi.pts, pi.ptsCount = t.PTS, 0 // no pts_count field in the schema
		pi.entry, pi.channel = mbEntry(t.ChannelID), true
		return pi, true
	case *gen.UpdatePinnedChannelMessages:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry, pi.channel = mbEntry(t.ChannelID), true
		return pi, true
	case *gen.UpdateChannelWebPage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		pi.entry, pi.channel = mbEntry(t.ChannelID), true
		return pi, true

	// ---- qts-carrying (secret entry; qts acts as pts with count 1) ---
	case *gen.UpdateNewEncryptedMessage:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		return pi, true
	case *gen.UpdateMessagePollVote:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		return pi, true
	case *gen.UpdateChatParticipant:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		return pi, true
	case *gen.UpdateChannelParticipant:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		pi.channel = true
		return pi, true
	case *gen.UpdateBotStopped:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		return pi, true
	case *gen.UpdateBotChatInviteRequester:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		return pi, true
	case *gen.UpdateBotChatBoost:
		pi.pts, pi.ptsCount, pi.entry = t.QTS, 1, mbSecret
		return pi, true

	// ---- short messages (arrive as whole Updates results; they carry
	// pts directly and are handled as account updates) -----------------
	case *gen.UpdateShortMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	case *gen.UpdateShortChatMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	case *gen.UpdateShortSentMessage:
		pi.pts, pi.ptsCount = t.PTS, t.PTSCount
		return pi, true
	}
	return pi, false
}

// entryFromMessagePeer resolves the channel entry from a message's peer
// (messagebox.py:110-113: update.message.peer_id.channel_id).
func entryFromMessagePeer(m gen.MessageClass) mbEntry {
	msg, ok := m.(*gen.Message)
	if !ok {
		return mbAccount
	}
	return entryFromPeer(msg.PeerID)
}

// entryFromPeer resolves the channel entry from a Peer (channels only;
// users and chats belong to the account entry).
func entryFromPeer(p gen.PeerClass) mbEntry {
	if c, ok := p.(*gen.PeerChannel); ok {
		return mbEntry(c.ChannelID)
	}
	return mbAccount
}
