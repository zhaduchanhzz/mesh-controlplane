package placement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
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

		target, err := lockClusters(tx, "id = ?", to)
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
		return Move{}, fmt.Errorf("%w: %w: move %s is no longer %s", ErrConflict, errMoveClosed, m.ID, from)
	}

	return loadMove(tx, m.ID)
}

// errMoveClosed reports a move whose state another call changed first (an
// operator's cancel under the scheduler): that call recorded the outcome.
var errMoveClosed = errors.New("move state changed concurrently")

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

			// P12c L7: owner AND epoch: back at the source at a later
			// epoch means handed over (and moved back), not cancellable.
			if tn.OwnerClusterID != m.FromClusterID || tn.OwnerEpoch != m.FromEpoch {
				return Move{}, "", fmt.Errorf("%w: tailnet %s was already handed over (owner %s at epoch %d)",
					ErrConflict, m.TailnetID, tn.OwnerClusterID, tn.OwnerEpoch)
			}

			// Withdraw this move's request; one already withdrawn (or
			// replaced by another request made by hand) is left alone.
			if tn.MovingToClusterID != nil && *tn.MovingToClusterID == m.ToClusterID {
				res := tx.Exec(`UPDATE tailnets SET moving_to_cluster_id = NULL, updated_at = ?
					WHERE id = ? AND owner_cluster_id = ? AND owner_epoch = ? AND moving_to_cluster_id = ? AND deleted_at IS NULL`,
					r.now(), m.TailnetID, m.FromClusterID, int64(m.FromEpoch), m.ToClusterID) //nolint:gosec // epochs are positive int64
				if res.Error != nil {
					return Move{}, "", fmt.Errorf("withdrawing move request: %w", res.Error)
				}

				if res.RowsAffected != 1 {
					return Move{}, "", fmt.Errorf("%w: the request of move %s changed concurrently", ErrConflict, moveID)
				}
			}
		case MoveHandedOver, MoveCancelled, MoveFailed:
			return Move{}, "", fmt.Errorf("%w: move %s is %s", ErrConflict, moveID, m.State)
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

		// Test hook (P12c C2): a cancel that lands after the scheduler took
		// the move as due, before its outcome is recorded.
		err := r.step("move-due")
		if err != nil {
			return rep, err
		}

		state, err := r.runDueMove(ctx, op, m)
		if errors.Is(err, errMoveClosed) {
			continue // cancelled meanwhile: the cancel recorded it
		}

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

		state, _, final := moveOutcome(tn, m)
		if !final {
			continue // still waiting for the hand-over
		}

		err = r.recordOutcome(ctx, op, m, state)
		if errors.Is(err, errMoveClosed) {
			continue // recorded meanwhile by the flip or a cancel, or not final any more
		}

		if err != nil {
			return rep, err
		}

		switch state {
		case MoveHandedOver:
			rep.HandedOver = append(rep.HandedOver, m.ID)
		case MoveCancelled:
			rep.Cancelled = append(rep.Cancelled, m.ID)
		case MoveFailed, MoveScheduled, MoveRequested:
			rep.Failed = append(rep.Failed, m.ID)
		default:
			rep.Failed = append(rep.Failed, m.ID)
		}
	}

	return rep, nil
}

// moveOutcome says what became of the REQUESTED move m, from tailnet tn,
// when the flip or the cancel did not record it (they do since P12c L7;
// this is the fallback, e.g. for a flip made by hand). It decides by the
// epoch, which only grows and names one owner: at m's epoch with m's
// request still there it is waiting (final false); at m's epoch without it,
// the request was withdrawn (CANCELLED); at the next epoch owned by m's
// target it was handed over; anything else cannot be told apart from the
// row alone and is reported FAILED, never guessed.
func moveOutcome(tn types.Tailnet, m Move) (MoveState, string, bool) {
	switch {
	case tn.OwnerEpoch == m.FromEpoch && tn.OwnerClusterID == m.FromClusterID:
		if tn.MovingToClusterID != nil && *tn.MovingToClusterID == m.ToClusterID {
			return MoveRequested, "", false
		}

		return MoveCancelled, "", true
	case tn.OwnerEpoch == m.FromEpoch+1 && tn.OwnerClusterID == m.ToClusterID:
		return MoveHandedOver, "", true
	default:
		return MoveFailed, fmt.Sprintf("tailnet is owned by %s at epoch %d (move requested at epoch %d from %s); "+
			"the outcome of this move was not recorded", tn.OwnerClusterID, tn.OwnerEpoch, m.FromEpoch, m.FromClusterID), true
	}
}

// recordOutcome records state, the outcome of the REQUESTED move m, under
// LockOwnership after re-reading the tailnet in the transaction, at the time
// it is recorded (never tailnets.updated_at, which other changes move). A
// move closed meanwhile, or whose outcome changed, is errMoveClosed.
func (r *Registry) recordOutcome(ctx context.Context, op Op, m Move, state MoveState) error {
	_, err := mutate(ctx, r, op, "MoveFinished", m.ID, map[string]any{"state": state},
		func(tx *gorm.DB) (Move, string, error) {
			err := tenantscope.LockOwnership(tx, m.TailnetID)
			if err != nil {
				return Move{}, "", err
			}

			tn, err := loadTailnet(tx, m.TailnetID)
			if err != nil {
				return Move{}, "", err
			}

			now, errText, final := moveOutcome(tn, m)
			if !final || now != state {
				return Move{}, "", fmt.Errorf("%w: move %s changed while recording it", errMoveClosed, m.ID)
			}

			out, err := r.setMoveState(tx, m, MoveRequested, state, errText, r.now())

			return out, m.TailnetID, err
		})

	return err
}

// runDueMove requests the hand-over of the due move m (MoveOwner, ADR 0010)
// and records it REQUESTED in ONE transaction under LockOwnership (P12c C2):
// a cancel (CancelScheduledMove takes the same lock) either comes first, and
// nothing is requested, or after, and withdraws the request it sees. A
// request already present for exactly this move (an operator ahead of the
// schedule) is adopted. A refused request (owner or epoch changed, target
// without a live lease or room) is recorded FAILED with the reason, again
// only while m is still SCHEDULED; nothing is retried behind the operator's
// back. Other errors (database, lock timeout) change nothing and are
// returned: the next pass tries again. A move closed by someone else
// meanwhile is errMoveClosed.
func (r *Registry) runDueMove(ctx context.Context, op Op, m Move) (MoveState, error) {
	_, err := mutate(ctx, r, op, "MoveDue", m.ID, map[string]any{"state": MoveRequested},
		func(tx *gorm.DB) (Move, string, error) {
			err := tenantscope.LockOwnership(tx, m.TailnetID)
			if err != nil {
				return Move{}, "", err
			}

			out, err := r.setMoveState(tx, m, MoveScheduled, MoveRequested, "", r.now())
			if err != nil {
				return Move{}, "", err
			}

			tn, err := loadTailnet(tx, m.TailnetID)
			if err != nil {
				return Move{}, "", refused(err)
			}

			if tn.MovingToClusterID != nil && *tn.MovingToClusterID == m.ToClusterID &&
				tn.OwnerClusterID == m.FromClusterID && tn.OwnerEpoch == m.FromEpoch {
				return out, m.TailnetID, nil // adopted
			}

			_, err = r.moveOwnerTx(tx, m.TailnetID, m.FromClusterID, m.FromEpoch, m.ToClusterID)
			if err != nil {
				return Move{}, "", refused(err)
			}

			err = r.auditTx(tx, op, "MoveOwner", m.TailnetID, map[string]any{
				"from_cluster": m.FromClusterID, "from_epoch": m.FromEpoch, "to_cluster": m.ToClusterID, "move": m.ID,
			})

			return out, m.TailnetID, err
		})
	if err == nil {
		return MoveRequested, nil
	}

	var ref refusal
	if !errors.As(err, &ref) {
		return "", err
	}

	errText := ref.err.Error()
	if errors.Is(ref.err, ErrStaleEpoch) {
		errText = "owner or epoch changed since the move was scheduled: " + errText
	}

	_, err = mutate(ctx, r, op, "MoveDue", m.ID, map[string]any{"state": MoveFailed, "error": errText},
		func(tx *gorm.DB) (Move, string, error) {
			err := tenantscope.LockOwnership(tx, m.TailnetID)
			if err != nil {
				return Move{}, "", err
			}

			out, err := r.setMoveState(tx, m, MoveScheduled, MoveFailed, errText, r.now())

			return out, m.TailnetID, err
		})
	if err != nil {
		return "", err
	}

	return MoveFailed, nil
}

// refusal marks why a due move cannot be requested (recorded as FAILED),
// as opposed to an error that changes nothing and is retried.
type refusal struct{ err error }

func (e refusal) Error() string { return e.err.Error() }
func (e refusal) Unwrap() error { return e.err }

// refused wraps err as a refusal when it is one: the tailnet, the target or
// the request itself does not allow the move.
func refused(err error) error {
	for _, e := range []error{ErrStaleEpoch, ErrNoCapacity, ErrConflict, ErrNotFound, ErrInvalid} {
		if errors.Is(err, e) {
			return refusal{err: err}
		}
	}

	return err
}

// auditTx writes an audit row with outcome ok in tx, beside the one mutate
// writes for the call (the scheduler's request keeps its MoveOwner row).
func (r *Registry) auditTx(tx *gorm.DB, op Op, action, target string, detail any) error {
	err := tx.Create(&auditRow{
		At: r.now(), Actor: op.Actor, Action: action, Target: target, Outcome: outcomeOK, Detail: marshalDetail(detail),
	}).Error
	if err != nil {
		return fmt.Errorf("writing audit: %w", err)
	}

	return nil
}

// SchedulerActor is the audit actor of the move scheduler.
const SchedulerActor = "spiffe://mesh/placement/scheduler"

// RunScheduler calls RunDueMoves every interval until ctx ends, as
// SchedulerActor. Errors are logged and retried on the next tick.
func RunScheduler(ctx context.Context, r *Registry, interval time.Duration, logf func(string, ...any)) {
	op := Op{Actor: SchedulerActor}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		rep, err := r.RunDueMoves(ctx, op)
		if err != nil && ctx.Err() == nil {
			logf("move scheduler: %v", err)
		} else if len(rep.Requested)+len(rep.HandedOver)+len(rep.Cancelled)+len(rep.Failed) > 0 {
			logf("move scheduler: requested %v handed over %v cancelled %v failed %v",
				rep.Requested, rep.HandedOver, rep.Cancelled, rep.Failed)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
