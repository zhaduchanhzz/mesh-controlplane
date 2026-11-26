package state

import "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"

// SetMoveNotice records the planned move of this runtime's tailnet (nil:
// none) and reports whether it changed (P12b). Full map responses carry it
// to the clients.
func (s *State) SetMoveNotice(n *types.MoveNotice) bool {
	old := s.moveNotice.Swap(n)

	if n != nil {
		s.moveNoticeSeen.Store(true)
	}

	return !old.Equal(n)
}

// MoveNotice returns the planned move of this runtime's tailnet, if any, and
// whether a notice was ever set (so a cleared one must be removed from the
// clients).
func (s *State) MoveNotice() (*types.MoveNotice, bool) {
	return s.moveNotice.Load(), s.moveNoticeSeen.Load()
}
