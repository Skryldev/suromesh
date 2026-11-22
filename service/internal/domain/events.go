package domain

// UpdateEvent is one accepted upstream update, delivered to subscribers
// after the account's MessageBox accepted it. Ordering: per account, in
// MessageBox-accepted order; own sends are silent (no subscriber event).
type UpdateEventKind int32

const (
	UpdateNewMessage      UpdateEventKind = 1
	UpdateEditedMessage   UpdateEventKind = 2
	UpdateDeletedMessages UpdateEventKind = 3
	UpdateReadState       UpdateEventKind = 4
)

// UpdateEvent is the union of supported update families. Exactly one
// variant is populated. StreamSeq is stamped per-subscriber by the
// fan-out hub (monotonic per stream; a gap means events were lost on
// that stream).
type UpdateEvent struct {
	Kind UpdateEventKind

	NewMessage *Message      // UpdateNewMessage
	Edited     *Message      // UpdateEditMessage
	Deleted    *DeletedBatch // UpdateDeleteMessages
	Read       *ReadState    // UpdateReadHistoryInbox/Outbox

	StreamSeq int64
}

// DeletedBatch describes a deletion event.
type DeletedBatch struct {
	Peer       PeerID
	MessageIDs []int64
}

// ReadState describes a read-state advance.
type ReadState struct {
	Peer     PeerID
	MaxID    int64
	Outgoing bool
}

// ConnectionEvent is a connection/authorization state transition.
type ConnectionEvent struct {
	Desired    DesiredState
	Observed   ObservedState
	ErrorClass string
}
