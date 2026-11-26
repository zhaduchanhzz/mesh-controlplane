package db

import (
	"context"
	"errors"
	"time"
)

// EpochFenceMigrationID (P13c, ADR 0012). RED stub: not in the chain yet.
const EpochFenceMigrationID = "202611271100-epoch-fence"

// EpochFenceRequest is one run of the epoch fence.
type EpochFenceRequest struct {
	Reason      string
	Operator    string
	OperatorNow time.Time
	ExternalHWM uint64
}

// EpochFenceResult says what the fence did.
type EpochFenceResult struct {
	Floor          uint64    `json:"floor"`
	Holder         string    `json:"holder"`
	Leases         int64     `json:"leases"`
	Tailnets       int64     `json:"tailnets"`
	MovesCancelled int64     `json:"moves_cancelled"`
	FencedAt       time.Time `json:"fenced_at"`
}

var errNotImplemented = errors.New("not implemented (P13c RED stub)")

// EpochFloor is the time-based epoch floor of ADR 0012 §1. RED stub.
func EpochFloor(time.Time) (uint64, error) { return 0, errNotImplemented }

// EpochFence raises every lease epoch and owner_epoch. RED stub.
func (p *Platform) EpochFence(context.Context, EpochFenceRequest) (EpochFenceResult, error) {
	return EpochFenceResult{}, errNotImplemented
}
