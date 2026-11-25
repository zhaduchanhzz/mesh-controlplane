//go:build !p05cluster

package servertest_test

import "testing"

// Without the cluster harness there are no two tailnets on one process.
func p08aTwoTailnets(t *testing.T) (url string, a, b p08aTailnet, ok bool) {
	t.Helper()

	return "", p08aTailnet{}, p08aTailnet{}, false
}
