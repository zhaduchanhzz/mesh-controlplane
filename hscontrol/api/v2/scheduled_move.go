package apiv2

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/scope"
)

func init() {
	registrations = append(registrations, registerScheduledMove)
}

// ScheduledMove is the planned move of the tailnet to another control
// cluster, as its own API keys see it (P12b, owner decision Q3): whether one
// is scheduled, when and why; never the target cluster. Read-only: moves are
// scheduled by the platform operator (mesh-placement).
type ScheduledMove struct {
	Scheduled   bool       `json:"scheduled"`
	ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

type (
	getScheduledMoveInput struct {
		Tailnet string `path:"tailnet"`
	}
	scheduledMoveOutput struct {
		Body ScheduledMove
	}
)

func registerScheduledMove(api huma.API, b Backend) {
	huma.Register(api, requireScope(huma.Operation{
		OperationID: "getTailnetScheduledMove",
		Method:      http.MethodGet,
		Path:        "/api/v2/tailnet/{tailnet}/scheduled-move",
		Summary:     "Get the planned move of the tailnet",
		Description: "Whether the platform operator scheduled a move of this tailnet to " +
			"another control cluster, when and why. The target is not shown: after the " +
			"move, clients ask bootstrap for their control URL and re-login. " +
			"Updated within one reconcile interval of the schedule.",
		Tags:     []string{"TailnetSettings"},
		Security: security,
		Errors:   []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
	}, scope.FeatureSettingsRead), func(ctx context.Context, in *getScheduledMoveInput) (*scheduledMoveOutput, error) {
		err := requireTailnet(b, in.Tailnet)
		if err != nil {
			return nil, err
		}

		out := &scheduledMoveOutput{}

		// The notice lives in this tailnet's own runtime state: another
		// tailnet's move is not reachable from here.
		if b.State == nil {
			return out, nil
		}

		if n, _ := b.State.MoveNotice(); n != nil {
			at := n.At.UTC()
			out.Body = ScheduledMove{Scheduled: true, ScheduledAt: &at, Reason: n.Reason}
		}

		return out, nil
	})
}
