package tenantscope

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/metricstest"
	"gorm.io/gorm"
)

// P13f: every refused tenant write is counted by why it was refused, so an
// operator sees a fence storm (a stale owner still writing, a lease lost
// under load) without reading logs. The reasons are fixed strings; no
// tailnet, cluster or holder is a label.
func TestFenceRefusalsCountedByReason(t *testing.T) {
	refusals := func(reason string) float64 {
		v, _ := metricstest.Value(t, "headscale_tenant_fence_refusals_total", map[string]string{"reason": reason})

		return v
	}

	t.Run("owner", func(t *testing.T) {
		db := openDB(t)

		err := db.Model(&tailnetRow{}).Where("id = ?", idA).Update("owner_epoch", int64(testEpoch+1)).Error
		if err != nil {
			t.Fatal(err)
		}

		before := refusals("owner")

		assertAllWritesFenced(t, db, ctxFor(t, idA), fencedCtx(t, idA, testCluster, testEpoch+1))

		if got := refusals("owner") - before; got < 5 {
			t.Errorf("owner refusals grew by %v, want >= 5", got)
		}
	})

	t.Run("not_active", func(t *testing.T) {
		db := openDB(t)

		err := db.Model(&tailnetRow{}).Where("id = ?", idA).Update("status", "SUSPENDED").Error
		if err != nil {
			t.Fatal(err)
		}

		before := refusals("not_active")

		assertAllWritesFenced(t, db, ctxFor(t, idA), ctxFor(t, idA))

		if got := refusals("not_active") - before; got < 5 {
			t.Errorf("not_active refusals grew by %v, want >= 5", got)
		}
	})

	t.Run("no_fence", func(t *testing.T) {
		db := openDB(t)
		before := refusals("no_fence")

		err := db.WithContext(scopeOnlyCtx(t, idA)).Model(&item{}).Where("name = ?", "x").Update("name", "y").Error
		if !errors.Is(err, ErrNoFence) {
			t.Fatalf("write without a fence: err=%v, want ErrNoFence", err)
		}

		if got := refusals("no_fence") - before; got != 1 {
			t.Errorf("no_fence refusals grew by %v, want 1", got)
		}
	})

	t.Run("lease", func(t *testing.T) {
		db, s := leaseDB(t)

		l, err := s.Acquire(context.Background(), testCluster, "a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		before := refusals("lease")

		// A holder that is not the one in the row.
		err = db.WithContext(leaseCtx(t, "b", l.Epoch)).Model(&item{}).Where("name = ?", "x").Update("name", "y").Error
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("write under another holder's lease: err=%v, want ErrFenced", err)
		}

		if got := refusals("lease") - before; got != 1 {
			t.Errorf("lease refusals grew by %v, want 1", got)
		}
	})
}

// P13f L10 (review P03b): GORM keeps one After per callback (the last
// call wins), so `.After(name+":create").After("gorm:begin_transaction")`
// declared only "after begin_transaction": the fence ran after the stamp
// only because it was registered later. The order must be declared: the
// scope/stamp callbacks after begin_transaction, and each fence after its
// scope callback, so begin_transaction < scope < fence < the GORM operation
// holds whatever order callbacks are registered in.
func TestFenceCallbacksDeclareTheirOrder(t *testing.T) {
	db := openDB(t)

	for _, op := range []struct {
		proc  string
		scope string
		gorm  string
	}{
		{"create", name + ":create", "gorm:create"},
		{"update", name + ":update", "gorm:update"},
		{"delete", name + ":delete", "gorm:delete"},
	} {
		t.Run(op.proc, func(t *testing.T) {
			p := processorOf(t, db, op.proc)
			fence := name + ":fence_" + op.proc

			cbs := callbacksOf(t, p)

			if got := cbs[fence].after; got != op.scope {
				t.Errorf("%s declares After(%q), want After(%q)", fence, got, op.scope)
			}

			if got := cbs[op.scope].after; got != "gorm:begin_transaction" {
				t.Errorf("%s declares After(%q), want After(%q)", op.scope, got, "gorm:begin_transaction")
			}

			order := effectiveOrder(t, p, cbs)
			pos := func(n string) int {
				for i, x := range order {
					if x == n {
						return i
					}
				}

				t.Fatalf("callback %q not in the %s chain %v", n, op.proc, order)

				return -1
			}

			if pos("gorm:begin_transaction") >= pos(op.scope) || pos(op.scope) >= pos(fence) || pos(fence) >= pos(op.gorm) {
				t.Errorf("%s chain %v: want begin_transaction < %s < %s < %s", op.proc, order, op.scope, fence, op.gorm)
			}
		})
	}
}

type declared struct {
	after   string
	handler uintptr
}

func processorOf(t *testing.T, db *gorm.DB, op string) reflect.Value {
	t.Helper()

	cb := db.Callback()

	var p any

	switch op {
	case "create":
		p = cb.Create()
	case "update":
		p = cb.Update()
	case "delete":
		p = cb.Delete()
	}

	return reflect.ValueOf(p).Elem()
}

// callbacksOf reads gorm's unexported callback list (name, after, handler)
// of processor p.
func callbacksOf(t *testing.T, p reflect.Value) map[string]declared {
	t.Helper()

	list := p.FieldByName("callbacks")
	if !list.IsValid() {
		t.Fatal("gorm processor has no callbacks field; update this test for the gorm version")
	}

	out := map[string]declared{}

	for i := range list.Len() {
		c := list.Index(i).Elem()
		out[c.FieldByName("name").String()] = declared{
			after:   c.FieldByName("after").String(),
			handler: c.FieldByName("handler").Pointer(),
		}
	}

	return out
}

// effectiveOrder maps the compiled chain (fns) back to callback names.
func effectiveOrder(t *testing.T, p reflect.Value, cbs map[string]declared) []string {
	t.Helper()

	byHandler := map[uintptr][]string{}
	for n, d := range cbs {
		byHandler[d.handler] = append(byHandler[d.handler], n)
	}

	fns := p.FieldByName("fns")

	var order []string

	seen := map[string]bool{}

	for i := range fns.Len() {
		// Several callbacks may share a handler (checkFence is registered
		// three times, each in its own processor; one per processor here).
		for _, n := range byHandler[fns.Index(i).Pointer()] {
			if !seen[n] {
				seen[n] = true
				order = append(order, n)

				break
			}
		}
	}

	return order
}
