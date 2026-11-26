package placement

import (
	"fmt"

	"tailscale.com/types/key"
)

// ValidateNoisePublicKey checks that s is a cluster Noise public key as
// clients see it on /key and the cluster records it (P12, P13e, ADR 0014
// §3): "mkey:" and 64 hex digits, not the zero key. A malformed value in
// the registry would match no process's key, so every process of the
// cluster would refuse to serve.
func ValidateNoisePublicKey(s string) error {
	var k key.MachinePublic

	err := k.UnmarshalText([]byte(s))
	if err != nil || k.IsZero() {
		return fmt.Errorf("%w: noise_public_key must be a Noise public key (mkey: and 64 hex digits)", ErrInvalid)
	}

	return nil
}
