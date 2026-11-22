package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// auditRow is a row of platform_audit.
type auditRow struct {
	ID             uint64 `gorm:"primaryKey"`
	At             time.Time
	Actor          string
	Action         string
	Target         string
	Outcome        string
	Detail         string
	IdempotencyKey *string
}

func (auditRow) TableName() string { return "platform_audit" }

// AuditEntry is one audit row as the operator API lists it.
type AuditEntry struct {
	ID             uint64    `json:"id"`
	At             time.Time `json:"at"`
	Actor          string    `json:"actor"`
	Action         string    `json:"action"`
	Target         string    `json:"target"`
	Outcome        string    `json:"outcome"`
	Detail         string    `json:"detail,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
}

// idempotencyRow is a row of platform_idempotency: the result of a
// successful mutation made with a key.
type idempotencyRow struct {
	Actor          string `gorm:"primaryKey"`
	IdempotencyKey string `gorm:"primaryKey"`
	Operation      string
	RequestHash    string
	Response       string
	CreatedAt      time.Time
}

func (idempotencyRow) TableName() string { return "platform_idempotency" }

const (
	outcomeOK     = "ok"
	outcomeReplay = "replay"
	outcomeError  = "error"
)

// maxKeyLen bounds an idempotency key.
const maxKeyLen = 200

func (op Op) validate() error {
	if op.Actor == "" {
		return fmt.Errorf("%w: mutation without an actor", ErrInvalid)
	}

	if len(op.IdempotencyKey) > maxKeyLen {
		return fmt.Errorf("%w: idempotency key longer than %d", ErrInvalid, maxKeyLen)
	}

	if op.IdempotencyKey != "" && op.RequestHash == "" {
		return fmt.Errorf("%w: idempotency key without a request hash", ErrInvalid)
	}

	return nil
}

func (op Op) keyPtr() *string {
	if op.IdempotencyKey == "" {
		return nil
	}

	k := op.IdempotencyKey

	return &k
}

// mutate runs fn in one transaction together with its audit row and, with
// an idempotency key, the stored result. A key already used by this actor
// returns the stored result (same request) or ErrIdempotencyConflict
// (another request), without calling fn. A failed call is audited with
// outcome "error" after the rollback. target names what the call acts on;
// fn may return a better one (the id of a created tailnet).
func mutate[T any](
	ctx context.Context, r *Registry, op Op, action, target string, detail any,
	fn func(tx *gorm.DB) (T, string, error),
) (T, error) {
	var zero T

	err := op.validate()
	if err != nil {
		return zero, err
	}

	detailJSON := marshalDetail(detail)

	if op.IdempotencyKey != "" {
		res, found, err := replay[T](ctx, r, op, action, target, detailJSON)
		if found || err != nil {
			return res, err
		}
	}

	var result T

	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res, tgt, err := fn(tx)
		if err != nil {
			return err
		}

		if tgt != "" {
			target = tgt
		}

		err = r.step("audit")
		if err != nil {
			return err
		}

		err = tx.Create(&auditRow{
			At: r.now(), Actor: op.Actor, Action: action, Target: target,
			Outcome: outcomeOK, Detail: detailJSON, IdempotencyKey: op.keyPtr(),
		}).Error
		if err != nil {
			return fmt.Errorf("writing audit: %w", err)
		}

		if op.IdempotencyKey != "" {
			err = r.step("idempotency")
			if err != nil {
				return err
			}

			body, err := json.Marshal(res)
			if err != nil {
				return fmt.Errorf("encoding result: %w", err)
			}

			err = tx.Create(&idempotencyRow{
				Actor: op.Actor, IdempotencyKey: op.IdempotencyKey, Operation: action,
				RequestHash: op.RequestHash, Response: string(body), CreatedAt: r.now(),
			}).Error
			if err != nil {
				return fmt.Errorf("storing idempotent result: %w", err)
			}
		}

		err = r.step("commit")
		if err != nil {
			return err
		}

		result = res

		return nil
	})
	if err == nil {
		return result, nil
	}

	// A concurrent call with the same key may have won: its stored result
	// is the answer (its effect is the one that happened).
	if op.IdempotencyKey != "" {
		res, found, rerr := replay[T](ctx, r, op, action, target, detailJSON)
		if found || rerr != nil {
			return res, rerr
		}
	}

	r.auditError(ctx, op, action, target, detailJSON, err)

	return zero, err
}

// replay returns the stored result of op's key, if any, auditing the replay
// or the conflict.
func replay[T any](ctx context.Context, r *Registry, op Op, action, target, detail string) (T, bool, error) {
	var (
		zero T
		rows []idempotencyRow
	)

	err := r.db.WithContext(ctx).
		Where("actor = ? AND idempotency_key = ?", op.Actor, op.IdempotencyKey).
		Limit(1).Find(&rows).Error
	if err != nil {
		return zero, false, fmt.Errorf("reading idempotency key: %w", err)
	}

	if len(rows) == 0 {
		return zero, false, nil
	}

	row := rows[0]
	if row.Operation != action || row.RequestHash != op.RequestHash {
		r.auditError(ctx, op, action, target, detail, ErrIdempotencyConflict)

		return zero, true, ErrIdempotencyConflict
	}

	var res T

	err = json.Unmarshal([]byte(row.Response), &res)
	if err != nil {
		return zero, true, fmt.Errorf("decoding stored result: %w", err)
	}

	r.audit(ctx, op, action, target, outcomeReplay, detail)

	return res, true, nil
}

func (r *Registry) auditError(ctx context.Context, op Op, action, target, detail string, cause error) {
	var d map[string]any
	if detail != "" {
		_ = json.Unmarshal([]byte(detail), &d)
	}

	if d == nil {
		d = map[string]any{}
	}

	d["error"] = cause.Error()
	r.audit(ctx, op, action, target, outcomeError, marshalDetail(d))
}

// audit writes an audit row outside a mutation (replays and failures). A
// failure to write it is returned to no one: the call's own result stands,
// and the row is best effort. Mutations themselves are audited in their
// transaction.
func (r *Registry) audit(ctx context.Context, op Op, action, target, outcome, detail string) {
	_ = r.db.WithContext(context.WithoutCancel(ctx)).Create(&auditRow{
		At: r.now(), Actor: op.Actor, Action: action, Target: target,
		Outcome: outcome, Detail: detail, IdempotencyKey: op.keyPtr(),
	}).Error
}

func marshalDetail(v any) string {
	if v == nil {
		return ""
	}

	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}

	return string(b)
}

// AuditLog returns the newest audit rows, newest first (limit 1..1000).
func (r *Registry) AuditLog(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: limit %d", ErrInvalid, limit)
	}

	var rows []auditRow

	err := r.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading audit: %w", err)
	}

	out := make([]AuditEntry, len(rows))
	for i, a := range rows {
		out[i] = AuditEntry{
			ID: a.ID, At: a.At, Actor: a.Actor, Action: a.Action, Target: a.Target,
			Outcome: a.Outcome, Detail: a.Detail,
		}
		if a.IdempotencyKey != nil {
			out[i].IdempotencyKey = *a.IdempotencyKey
		}
	}

	return out, nil
}

// isDuplicate reports a uniqueness violation (gorm translates driver errors).
func isDuplicate(err error) bool { return errors.Is(err, gorm.ErrDuplicatedKey) }
