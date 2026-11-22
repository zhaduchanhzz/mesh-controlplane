package placement

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// RED phase: signatures only.

var errNotImplemented = errors.New("not implemented")

func (r *Registry) CreateCluster(ctx context.Context, op Op, spec ClusterSpec) (Cluster, error) {
	return Cluster{}, errNotImplemented
}

func (r *Registry) UpdateCluster(ctx context.Context, op Op, id string, p ClusterPatch) (Cluster, error) {
	return Cluster{}, errNotImplemented
}

func (r *Registry) UpdateClusterState(ctx context.Context, op Op, id string, state ClusterState, expectedRevision uint64) (Cluster, error) {
	return Cluster{}, errNotImplemented
}

func (r *Registry) DeleteCluster(ctx context.Context, op Op, id string) error {
	return errNotImplemented
}

func (r *Registry) GetCluster(ctx context.Context, id string) (ClusterUsage, error) {
	return ClusterUsage{}, errNotImplemented
}

func (r *Registry) ListClusters(ctx context.Context) ([]ClusterUsage, error) {
	return nil, errNotImplemented
}

func (r *Registry) PlaceTailnet(ctx context.Context, class Class) (Cluster, error) {
	return Cluster{}, errNotImplemented
}

func (r *Registry) CreateTailnet(ctx context.Context, op Op, slug string, class Class) (types.Tailnet, error) {
	return types.Tailnet{}, errNotImplemented
}

func (r *Registry) ActivateTailnet(ctx context.Context, op Op, id string, expectedEpoch uint64) (types.Tailnet, error) {
	return types.Tailnet{}, errNotImplemented
}

func (r *Registry) SuspendTailnet(ctx context.Context, op Op, id string, expectedEpoch uint64) (types.Tailnet, error) {
	return types.Tailnet{}, errNotImplemented
}

func (r *Registry) GetTailnet(ctx context.Context, id string) (TailnetInfo, error) {
	return TailnetInfo{}, errNotImplemented
}

func (r *Registry) MoveOwner(ctx context.Context, op Op, id, fromCluster string, fromEpoch uint64, toCluster string) (types.Tailnet, error) {
	return types.Tailnet{}, errNotImplemented
}

func (r *Registry) EnrollmentAllowed(ctx context.Context, id tailnet.ID) (Quota, error) {
	return Quota{}, errNotImplemented
}

func (r *Registry) EnrollmentAllowedTx(tx *gorm.DB, id tailnet.ID) (Quota, error) {
	return Quota{}, errNotImplemented
}

// TailnetInfo is a tailnet with its live device count.
type TailnetInfo struct {
	types.Tailnet

	Devices int64 `json:"devices"`
}
