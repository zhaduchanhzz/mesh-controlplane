package placement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// P12b (owner decision Q2, 2026-11-26): planned moves of a tailnet to
// another cluster. An operator schedules a move; mesh-placement requests it
// (MoveOwner, ADR 0010) at scheduled_at; the owner cluster hands the tailnet
// over; the schedule records each step and its time. tailnet_moves is read
// and written with explicit SQL only: its tailnet_id names the tailnet being
// moved, it is not a tenant scope (tenantscope must not filter it).

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

// RunReport says what one RunDueMoves pass did (move IDs).
type RunReport struct {
	Requested  []string `json:"requested,omitempty"`
	HandedOver []string `json:"handed_over,omitempty"`
	Cancelled  []string `json:"cancelled,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

const moveCols = `id, tailnet_id, from_cluster_id, to_cluster_id, from_epoch, scheduled_at, reason, state, error,
	requested_at, finished_at, created_at, updated_at`

func queryMoves(db *gorm.DB, where string, args ...any) ([]Move, error) {
	rows, err := db.Raw(`SELECT `+moveCols+` FROM tailnet_moves WHERE `+where, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("reading tailnet moves: %w", err)
	}
	defer rows.Close()

	var out []Move

	for rows.Next() {
		var (
			m        Move
			epoch    int64
			state    string
			req, fin sql.NullTime
		)

		err = rows.Scan(&m.ID, &m.TailnetID, &m.FromClusterID, &m.ToClusterID, &epoch, &m.ScheduledAt, &m.Reason,
			&state, &m.Error, &req, &fin, &m.CreatedAt, &m.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("reading tailnet moves: %w", err)
		}

		m.FromEpoch = uint64(epoch) //nolint:gosec // CHECK(from_epoch >= 1)
		m.State = MoveState(state)

		if req.Valid {
			t := req.Time.UTC()
			m.RequestedAt = &t
		}

		if fin.Valid {
			t := fin.Time.UTC()
			m.FinishedAt = &t
		}

		m.ScheduledAt, m.CreatedAt, m.UpdatedAt = m.ScheduledAt.UTC(), m.CreatedAt.UTC(), m.UpdatedAt.UTC()
		out = append(out, m)
	}

	return out, rows.Err()
}

func loadMove(db *gorm.DB, id string) (Move, error) {
	ms, err := queryMoves(db, `id = ?`, id)
	if err != nil {
		return Move{}, err
	}

	if len(ms) == 0 {
		return Move{}, fmt.Errorf("%w: move %q", ErrNotFound, id)
	}

	return ms[0], nil
}

// ScheduleMove plans the move of tailnet id to cluster to at time at. The
// tailnet must be ACTIVE with no move open; to must be registered, ACTIVE
// and not the owner. The owner and epoch now are recorded: if either has
// changed when the move is due, it fails instead of moving someone else's
// state. The target's lease and capacity are checked when the move runs.
func (r *Registry) ScheduleMove(ctx context.Context, op Op, id, to string, at time.Time, reason string) (Move, error) {
	switch {
	case to == "":
		return Move{}, fmt.Errorf("%w: target cluster is required", ErrInvalid)
	case len(reason) > MaxMoveReasonLen:
		return Move{}, fmt.Errorf("%w: reason longer than %d", ErrInvalid, MaxMoveReasonLen)
	case at.Before(r.now()):
		return Move{}, fmt.Errorf("%w: scheduled time is in the past", ErrInvalid)
	}

	detail := map[string]any{"to_cluster": to, "scheduled_at": at.UTC(), "reason": reason}

	return mutate(ctx, r, op, "ScheduleMove", id, detail, func(tx *gorm.DB) (Move, string, error) {
		err := tenantscope.LockOwnership(tx, id)
		if err != nil {
			return Move{}, "", err
		}

		tn, err := loadTailnet(tx, id)
		if err != nil {
			return Move{}, "", err
		}

		if tn.Status != types.TailnetActive {
			return Move{}, "", fmt.Errorf("%w: tailnet %s is %s", ErrInvalidTransition, id, tn.Status)
		}

		if to == tn.OwnerClusterID {
			return Move{}, "", fmt.Errorf("%w: tailnet %s is already owned by %s", ErrInvalid, id, to)
		}

		target, err := lockClusters(tx, "SHARE", "id = ?", to)
		if err != nil {
			return Move{}, "", err
		}

		if target[0].State != ClusterActive {
			return Move{}, "", fmt.Errorf("%w: cluster %s is %s", ErrNoCapacity, to, target[0].State)
		}

		open, err := queryMoves(tx, `tailnet_id = ? AND state IN ('SCHEDULED','REQUESTED')`, id)
		if err != nil {
			return Move{}, "", err
		}

		if len(open) > 0 || tn.MovingToClusterID != nil {
			return Move{}, "", fmt.Errorf("%w: a move of tailnet %s is already open", ErrConflict, id)
		}

		now := r.now().UTC()
		m := Move{
			ID: uuid.NewV4().String(), TailnetID: id, FromClusterID: tn.OwnerClusterID, ToClusterID: to,
			FromEpoch: tn.OwnerEpoch, ScheduledAt: at.UTC(), Reason: reason, State: MoveScheduled,
			CreatedAt: now, UpdatedAt: now,
		}

		err = tx.Exec(`INSERT INTO tailnet_moves (id, tailnet_id, from_cluster_id, to_cluster_id, from_epoch,
			scheduled_at, reason, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.TailnetID, m.FromClusterID, m.ToClusterID, int64(m.FromEpoch), //nolint:gosec // epochs are positive int64
			m.ScheduledAt, m.Reason, string(m.State), now, now).Error
		if err != nil {
			return Move{}, "", fmt.Errorf("scheduling move: %w", err)
		}

		return m, "", nil
	})
}

// setMoveState moves m from state from to state to (with requested_at or
// finished_at and an error text); exactly one row must change.
func (r *Registry) setMoveState(tx *gorm.DB, m Move, from, to MoveState, errText string, at time.Time) (Move, error) {
	col := "finished_at"
	if to == MoveRequested {
		col = "requested_at"
	}

	res := tx.Exec(`UPDATE tailnet_moves SET state = ?, error = ?, `+col+` = ?, updated_at = ?
		WHERE id = ? AND state = ?`, string(to), errText, at.UTC(), r.now().UTC(), m.ID, string(from))
	if res.Error != nil {
		return Move{}, fmt.Errorf("updating move: %w", res.Error)
	}

	if res.RowsAffected != 1 {
		return Move{}, fmt.Errorf("%w: move %s is no longer %s", ErrConflict, m.ID, from)
	}

	return loadMove(tx, m.ID)
}

// CancelScheduledMove cancels move moveID before its hand-over. A REQUESTED
// move also withdraws the request on the tailnet (as CancelMove). A move
// already handed over, cancelled or failed is ErrConflict.
func (r *Registry) CancelScheduledMove(ctx context.Context, op Op, moveID string) (Move, error) {
	return mutate(ctx, r, op, "CancelScheduledMove", moveID, nil, func(tx *gorm.DB) (Move, string, error) {
		m, err := loadMove(tx, moveID)
		if err != nil {
			return Move{}, "", err
		}

		err = tenantscope.LockOwnership(tx, m.TailnetID)
		if err != nil {
			return Move{}, "", err
		}

		switch m.State {
		case MoveScheduled:
		case MoveRequested:
			tn, err := loadTailnet(tx, m.TailnetID)
			if err != nil {
				return Move{}, "", err
			}

			if tn.OwnerClusterID != m.FromClusterID {
				return Move{}, "", fmt.Errorf("%w: tailnet %s was already handed over", ErrConflict, m.TailnetID)
			}

			res := tx.Exec(`UPDATE tailnets SET moving_to_cluster_id = NULL, updated_at = ?
				WHERE id = ? AND owner_cluster_id = ? AND moving_to_cluster_id = ? AND deleted_at IS NULL`,
				r.now(), m.TailnetID, m.FromClusterID, m.ToClusterID)
			if res.Error != nil {
				return Move{}, "", fmt.Errorf("withdrawing move request: %w", res.Error)
			}
		default:
			return Move{}, "", fmt.Errorf("%w: move %s is %s", ErrConflict, moveID, m.State)
		}

		out, err := r.setMoveState(tx, m, m.State, MoveCancelled, "", r.now())

		return out, m.TailnetID, err
	})
}

// GetMove returns move moveID.
func (r *Registry) GetMove(ctx context.Context, moveID string) (Move, error) {
	return loadMove(r.db.WithContext(ctx), moveID)
}

// ListMoves returns the moves of tailnet id, newest first.
func (r *Registry) ListMoves(ctx context.Context, id string) ([]Move, error) {
	return queryMoves(r.db.WithContext(ctx), `tailnet_id = ? ORDER BY created_at DESC, id`, id)
}

// PendingMove returns the open (SCHEDULED or REQUESTED) move of tailnet id.
func (r *Registry) PendingMove(ctx context.Context, id string) (Move, bool, error) {
	ms, err := queryMoves(r.db.WithContext(ctx), `tailnet_id = ? AND state IN ('SCHEDULED','REQUESTED')`, id)
	if err != nil || len(ms) == 0 {
		return Move{}, false, err
	}

	return ms[0], true, nil
}

// RunDueMoves requests the moves whose time has come (MoveOwner, ADR 0010)
// and records the outcome of requested ones. A request already present on
// the tailnet for exactly this move (a crash between the two steps, or an
// operator ahead of the schedule) is adopted. A move that cannot be
// requested (owner or epoch changed, target without a live lease or room)
// fails with the reason; nothing is retried behind the operator's back.
// mesh-placement calls it periodically; each step is audited.
func (r *Registry) RunDueMoves(ctx context.Context, op Op) (RunReport, error) {
	var rep RunReport

	db := r.db.WithContext(ctx)

	scheduled, err := queryMoves(db, `state = 'SCHEDULED'`)
	if err != nil {
		return rep, err
	}

	now := r.now()

	for _, m := range scheduled {
		if m.ScheduledAt.After(now) {
			continue
		}

		state, errText := r.requestMove(ctx, op, m)

		_, err := mutate(ctx, r, op, "MoveDue", m.ID, map[string]any{"state": state, "error": errText},
			func(tx *gorm.DB) (Move, string, error) {
				out, err := r.setMoveState(tx, m, MoveScheduled, state, errText, r.now())

				return out, m.TailnetID, err
			})
		if err != nil {
			return rep, err
		}

		if state == MoveRequested {
			rep.Requested = append(rep.Requested, m.ID)
		} else {
			rep.Failed = append(rep.Failed, m.ID)
		}
	}

	requested, err := queryMoves(db, `state = 'REQUESTED'`)
	if err != nil {
		return rep, err
	}

	for _, m := range requested {
		tn, err := loadTailnet(db, m.TailnetID)
		if err != nil {
			return rep, err
		}

		var (
			state   MoveState
			errText string
			at      = r.now()
		)

		switch {
		case tn.OwnerClusterID == m.ToClusterID && tn.MovingToClusterID == nil:
			state, at = MoveHandedOver, tn.UpdatedAt
		case tn.OwnerClusterID == m.FromClusterID && tn.MovingToClusterID == nil:
			state = MoveCancelled
		case tn.MovingToClusterID != nil && *tn.MovingToClusterID != m.ToClusterID,
			tn.OwnerClusterID != m.FromClusterID && tn.OwnerClusterID != m.ToClusterID:
			state, errText = MoveFailed, fmt.Sprintf("tailnet moved elsewhere (owner %s)", tn.OwnerClusterID)
		default:
			continue // still waiting for the hand-over
		}

		_, err = mutate(ctx, r, op, "MoveFinished", m.ID, map[string]any{"state": state},
			func(tx *gorm.DB) (Move, string, error) {
				out, err := r.setMoveState(tx, m, MoveRequested, state, errText, at)

				return out, m.TailnetID, err
			})
		if err != nil {
			return rep, err
		}

		switch state {
		case MoveHandedOver:
			rep.HandedOver = append(rep.HandedOver, m.ID)
		case MoveCancelled:
			rep.Cancelled = append(rep.Cancelled, m.ID)
		default:
			rep.Failed = append(rep.Failed, m.ID)
		}
	}

	return rep, nil
}

// requestMove asks for the hand-over of m and says which state m moves to.
func (r *Registry) requestMove(ctx context.Context, op Op, m Move) (MoveState, string) {
	tn, err := loadTailnet(r.db.WithContext(ctx), m.TailnetID)
	if err != nil {
		return MoveFailed, err.Error()
	}

	if tn.MovingToClusterID != nil && *tn.MovingToClusterID == m.ToClusterID &&
		tn.OwnerClusterID == m.FromClusterID && tn.OwnerEpoch == m.FromEpoch {
		return MoveRequested, "" // adopted
	}

	_, err = r.MoveOwner(ctx, op, m.TailnetID, m.FromClusterID, m.FromEpoch, m.ToClusterID)
	if err != nil {
		if errors.Is(err, ErrStaleEpoch) {
			return MoveFailed, "owner or epoch changed since the move was scheduled: " + err.Error()
		}

		return MoveFailed, err.Error()
	}

	return MoveRequested, ""
}
