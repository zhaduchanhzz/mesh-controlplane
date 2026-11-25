//go:build !p05cluster

package servertest_test

import "testing"

// Without the cluster harness there are no two tailnets on one process.
func p08aTwoTailnets(t *testing.T) (string, p08aTailnet, p08aTailnet, bool) {
	t.Helper()

	return "", p08aTailnet{}, p08aTailnet{}, false
}
