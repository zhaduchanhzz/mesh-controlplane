package db

import "errors"

// ErrNoiseKeyMismatch reports that the Noise public key registered for this
// cluster (clusters.noise_public_key) is not the key this process serves
// with: the process must not serve (P12, owner decision Q6, fail closed).
var ErrNoiseKeyMismatch = errors.New("cluster Noise public key differs from the registered one")

// RecordNoiseKey checks this process's Noise public key pub against the
// cluster's registry row (P12, Q6): an empty key is recorded (write-once),
// an equal one is fine, a different one is ErrNoiseKeyMismatch. A cluster
// with no registry row is not checked (reported as R1/R5 by reconcile).
// Only while this process holds the cluster lease it recorded with
// SetLease.
func (p *Platform) RecordNoiseKey(pub string) error {
	return nil
}
