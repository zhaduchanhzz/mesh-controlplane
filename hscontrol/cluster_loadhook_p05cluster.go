//go:build p05cluster

package hscontrol

import "github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"

// SetLoadHook makes c call f between building a tailnet's runtime and
// starting it, so a test can send requests in that window (P12c B5). Built
// only with the p05cluster tag (servertest); nil removes the hook.
func (c *Cluster) SetLoadHook(f func(tailnet.ID)) {
	c.mu.Lock()
	c.loadHook = f
	c.mu.Unlock()
}
