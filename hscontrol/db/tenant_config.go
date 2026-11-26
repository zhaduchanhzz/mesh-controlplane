package db

import (
	"errors"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// A tenant's own policy and DNS writes through its runtime's handle (P12d).

var errTenantConfigNotImplemented = errors.New("not implemented")

// SetPolicyRevised stores policy and bumps the tailnet's policy_revision.
func (hsdb *HSDatabase) SetPolicyRevised(policy string) (*types.Policy, uint64, error) {
	return nil, 0, errTenantConfigNotImplemented
}

// TenantDNS returns the stored DNS settings of the handle's tailnet.
func (hsdb *HSDatabase) TenantDNS() (*types.TenantDNS, error) {
	return nil, errTenantConfigNotImplemented
}

// UpdateTenantDNS replaces the handle's tailnet's DNS settings by fn's.
func (hsdb *HSDatabase) UpdateTenantDNS(
	fn func(*types.TenantDNS) (*types.TenantDNS, error),
) (*types.TenantDNS, uint64, error) {
	return nil, 0, errTenantConfigNotImplemented
}
