package placement

import (
	"context"
	"errors"
	"time"
)

// P12b (owner decisions Q2, 2026-11-26): planned moves of a tailnet to
// another cluster. A move is scheduled by an operator; mesh-placement
// requests it (MoveOwner, ADR 0010) at scheduled_at; the owner cluster hands
// the tailnet over; the schedule records each step and its time.

// MoveState is the state of a planned move.
type MoveState string

const (
	MoveScheduled  MoveState = "SCHEDULED"
	MoveRequested  MoveState = "REQUESTED"
	MoveHandedOver MoveState = "HANDED_OVER"
	MoveCancelled  MoveState = "CANCELLED"
	MoveFailed     MoveState = "FAILED"
)

// Move is one planned move.
type Move struct {
	ID            string     `json:"id"`
	TailnetID     string     `json:"tailnet_id"`
	FromClusterID string     `json:"from_cluster_id"`
	ToClusterID   string     `json:"to_cluster_id"`
	FromEpoch     uint64     `json:"from_epoch"`
	ScheduledAt   time.Time  `json:"scheduled_at"`
	Reason        string     `json:"reason,omitempty"`
	State         MoveState  `json:"state"`
	Error         string     `json:"error,omitempty"`
	RequestedAt   *time.Time `json:"requested_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// MaxMoveReasonLen bounds Move.Reason.
const MaxMoveReasonLen = 256

// RunReport says what one RunDueMoves pass did.
type RunReport struct {
	Requested  []string `json:"requested,omitempty"`
	HandedOver []string `json:"handed_over,omitempty"`
	Cancelled  []string `json:"cancelled,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

var errMovesNotImplemented = errors.New("not implemented")

// ScheduleMove plans the move of tailnet id to cluster to at time at.
func (r *Registry) ScheduleMove(ctx context.Context, op Op, id, to string, at time.Time, reason string) (Move, error) {
	return Move{}, errMovesNotImplemented
}

// CancelScheduledMove cancels move moveID before its hand-over.
func (r *Registry) CancelScheduledMove(ctx context.Context, op Op, moveID string) (Move, error) {
	return Move{}, errMovesNotImplemented
}

// GetMove returns move moveID.
func (r *Registry) GetMove(ctx context.Context, moveID string) (Move, error) {
	return Move{}, errMovesNotImplemented
}

// ListMoves returns the moves of tailnet id, newest first.
func (r *Registry) ListMoves(ctx context.Context, id string) ([]Move, error) {
	return nil, errMovesNotImplemented
}

// PendingMove returns the open (SCHEDULED or REQUESTED) move of tailnet id.
func (r *Registry) PendingMove(ctx context.Context, id string) (Move, bool, error) {
	return Move{}, false, errMovesNotImplemented
}

// RunDueMoves requests the moves whose time has come and records the
// outcome of requested ones. mesh-placement calls it periodically.
func (r *Registry) RunDueMoves(ctx context.Context, op Op) (RunReport, error) {
	return RunReport{}, errMovesNotImplemented
}
