package types

// SameNode reports whether v and o view the same node value in memory. The
// NodeStore keeps a node's view across snapshots until the node is written
// (P14f F4), so for views taken from NodeStore snapshots SameNode means "this
// node has not changed since o was taken". Views made elsewhere (GetNode,
// Node.View on a copy) are never the same as a snapshot view; callers only
// lose sharing then, never correctness.
func (v NodeView) SameNode(o NodeView) bool {
	return v.ж != nil && v.ж == o.ж
}
