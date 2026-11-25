package types

import "time"

// MoveNotice is the planned move of a tailnet to another cluster, as its
// clients are told (P12b, owner decision Q1): the time and the operator's
// reason, never the target cluster.
type MoveNotice struct {
	At     time.Time
	Reason string
}

// Equal reports whether n and o carry the same notice (both nil included).
func (n *MoveNotice) Equal(o *MoveNotice) bool {
	if n == nil || o == nil {
		return n == o
	}

	return n.At.Equal(o.At) && n.Reason == o.Reason
}
