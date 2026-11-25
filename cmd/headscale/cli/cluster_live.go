package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// Live administration of a RUNNING cluster (P08b): the commands below call
// the admin API of the cluster process on its unix socket (unix_socket of
// the config), so the change takes effect in the runtime serving the
// tailnet and reaches its clients at once. Run them on the host of the
// active process; a standby answers 503. The tailnet is --slug, resolved to
// its ID on the platform database.

var (
	clusterLiveNodeID uint64
	clusterLiveRoutes []string
	clusterLiveUser   string
)

var errClusterLive = errors.New("cluster admin")

func init() {
	for _, c := range []*cobra.Command{tailnetNodeDeleteCmd, tailnetNodeExpireCmd, tailnetRoutesApproveCmd} {
		c.Flags().Uint64Var(&clusterLiveNodeID, "id", 0, "node ID (from `tailnet node list`)")
	}

	tailnetRoutesApproveCmd.Flags().StringSliceVar(&clusterLiveRoutes, "routes", nil,
		"routes to approve, comma separated (replaces the approved set; empty approves none)")
	tailnetUserDeleteCmd.Flags().StringVar(&clusterLiveUser, "name", "", "user name")

	tailnetNodeCmd.AddCommand(tailnetNodeListCmd, tailnetNodeDeleteCmd, tailnetNodeExpireCmd)
	tailnetRoutesCmd.AddCommand(tailnetRoutesListCmd, tailnetRoutesApproveCmd)
	tailnetUserCmd.AddCommand(tailnetUserDeleteCmd)
	tailnetCmd.AddCommand(tailnetNodeCmd, tailnetRoutesCmd, tailnetUserCmd)
}

var tailnetNodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Nodes of tailnet --slug in the running cluster (unix socket)",
}

var tailnetRoutesCmd = &cobra.Command{
	Use:   "routes",
	Short: "Subnet routes of tailnet --slug in the running cluster (unix socket)",
}

var tailnetUserCmd = &cobra.Command{
	Use:   "user",
	Short: "Users of tailnet --slug in the running cluster (unix socket)",
}

var tailnetNodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the nodes the running cluster serves for tailnet --slug",
	RunE: func(_ *cobra.Command, _ []string) error {
		return liveCall(http.MethodGet, "/nodes", nil)
	},
}

var tailnetRoutesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List nodes with their announced and approved routes",
	RunE: func(_ *cobra.Command, _ []string) error {
		return liveCall(http.MethodGet, "/nodes", nil)
	},
}

var tailnetNodeDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete node --id; its peers drop it at once",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterLiveNodeID == 0 {
			return fmt.Errorf("%w: --id is required", errClusterLive)
		}

		return liveCall(http.MethodDelete, fmt.Sprintf("/node/%d", clusterLiveNodeID), nil)
	},
}

var tailnetNodeExpireCmd = &cobra.Command{
	Use:   "expire",
	Short: "Expire node --id now; it must log in again",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterLiveNodeID == 0 {
			return fmt.Errorf("%w: --id is required", errClusterLive)
		}

		return liveCall(http.MethodPost, fmt.Sprintf("/node/%d/expire", clusterLiveNodeID), nil)
	},
}

var tailnetRoutesApproveCmd = &cobra.Command{
	Use:   "approve",
	Short: "Set the approved routes of node --id",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterLiveNodeID == 0 {
			return fmt.Errorf("%w: --id is required", errClusterLive)
		}

		routes := clusterLiveRoutes
		if routes == nil {
			routes = []string{}
		}

		return liveCall(http.MethodPost, fmt.Sprintf("/node/%d/routes", clusterLiveNodeID),
			map[string]any{"routes": routes})
	},
}

var tailnetUserDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete user --name (refused while it owns nodes)",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterLiveUser == "" || strings.ContainsAny(clusterLiveUser, "/?#") {
			return fmt.Errorf("%w: --name is required and is a plain user name", errClusterLive)
		}

		return liveCall(http.MethodDelete, "/user/"+clusterLiveUser, nil)
	},
}

// liveCall resolves --slug to its tailnet ID and calls the admin API of the
// local cluster process; the JSON answer is printed.
func liveCall(method, rest string, body any) error {
	cfg, err := clusterConfig()
	if err != nil {
		return err
	}

	if cfg.UnixSocket == "" {
		return fmt.Errorf("%w: unix_socket is not configured", errClusterLive)
	}

	var id tailnet.ID

	err = withPlatform(func(p *db.Platform) error {
		tn, err := p.TailnetBySlug(clusterSlug)
		if err != nil {
			return err
		}

		id, err = tailnet.ParseID(tn.ID)

		return err
	})
	if err != nil {
		return err
	}

	var rd io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}

		rd = bytes.NewReader(b)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) //nolint:mnd
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, "http://cluster-admin/cluster/v1/tailnet/"+id.String()+rest, rd)
	if err != nil {
		return err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	socket := cfg.UnixSocket
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return new(net.Dialer).DialContext(ctx, "unix", socket)
		},
	}}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: is the cluster running here? %w", errClusterLive, err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) //nolint:mnd
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}

		_ = json.Unmarshal(out, &e)

		return fmt.Errorf("%w: %s: %s", errClusterLive, resp.Status, e.Error)
	}

	var v any
	if err := json.Unmarshal(out, &v); err != nil { //nolint:noinlineerr
		return fmt.Errorf("%w: answer is not JSON: %w", errClusterLive, err)
	}

	return printJSON(v)
}
