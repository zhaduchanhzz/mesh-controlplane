package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// `headscale cluster` runs one cluster process serving many tailnets (ADR
// 0005, ADR 0007) and, until the registry (P10), administers its tailnets
// directly on the database. The admin subcommands are operator tools: run
// them while the cluster is stopped, since a running cluster loads its
// tailnets, policies and users at start.

var (
	clusterID           string
	clusterSlug         string
	clusterStatus       string
	clusterPolicyFile   string
	clusterUser         string
	clusterKeyExpiry    time.Duration
	clusterKeyReusable  bool
	clusterKeyEphemeral bool
	clusterMoveTo       string
)

var errClusterIDRequired = errors.New("--cluster-id is required")

func init() {
	clusterCmd.PersistentFlags().StringVar(&clusterID, "cluster-id", "", "ID of the cluster this process serves (required)")

	tailnetCmd.PersistentFlags().StringVar(&clusterSlug, "slug", "", "tailnet slug")

	tailnetStatusCmd.Flags().StringVar(&clusterStatus, "set", "", "new status: PENDING, ACTIVE, SUSPENDED or DELETING")
	tailnetPolicyCmd.Flags().StringVar(&clusterPolicyFile, "file", "", "policy file (HuJSON)")
	tailnetAuthKeyCmd.Flags().StringVar(&clusterUser, "user", "", "user owning the nodes (created if missing)")
	tailnetAuthKeyCmd.Flags().DurationVar(&clusterKeyExpiry, "expiration", time.Hour, "key lifetime")
	tailnetAuthKeyCmd.Flags().BoolVar(&clusterKeyReusable, "reusable", false, "key can register several nodes")
	tailnetAuthKeyCmd.Flags().BoolVar(&clusterKeyEphemeral, "ephemeral", false, "nodes are ephemeral")
	tailnetReassignCmd.Flags().StringVar(&clusterMoveTo, "to", "", "cluster that takes ownership (this cluster again = new epoch)")

	tailnetCmd.AddCommand(tailnetCreateCmd, tailnetStatusCmd, tailnetPolicyCmd, tailnetAuthKeyCmd, tailnetNodesCmd, tailnetReassignCmd)
	clusterCmd.AddCommand(clusterServeCmd, tailnetCmd)
	rootCmd.AddCommand(clusterCmd)
}

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Serve and administer one cluster process with many tailnets",
}

func clusterConfig() (*types.Config, error) {
	if clusterID == "" {
		return nil, errClusterIDRequired
	}

	cfg := serverConfig()
	cfg.ClusterID = clusterID
	cfg.TailnetID = ""
	cfg.PolicyEmptyDenies = true

	return cfg, nil
}

var clusterServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve every ACTIVE tailnet this cluster owns on one listener and Noise key",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := clusterConfig()
		if err != nil {
			return err
		}

		c, err := hscontrol.NewCluster(cfg)
		if err != nil {
			return fmt.Errorf("initializing cluster: %w", err)
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		defer stop()

		err = c.Serve(ctx)
		if err != nil {
			return classifyServeError(err)
		}

		return nil
	},
}

var tailnetCmd = &cobra.Command{
	Use:   "tailnet",
	Short: "Administer the cluster's tailnets on the database (a running cluster follows within seconds)",
}

// withPlatform opens the platform database for one admin command.
func withPlatform(fn func(p *db.Platform) error) error {
	cfg, err := clusterConfig()
	if err != nil {
		return err
	}

	p, err := db.OpenPlatform(cfg)
	if err != nil {
		return err
	}
	defer p.Close()

	return fn(p)
}

// withTenant opens the database handle of the ACTIVE tailnet --slug.
func withTenant(fn func(id tailnet.ID, hs *db.HSDatabase) error) error {
	return withPlatform(func(p *db.Platform) error {
		tn, err := p.TailnetBySlug(clusterSlug)
		if err != nil {
			return err
		}

		id, err := tailnet.ParseID(tn.ID)
		if err != nil {
			return err
		}

		hs, err := p.TenantDB(id)
		if err != nil {
			return err
		}

		return fn(id, hs)
	})
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(v)
}

var tailnetCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create tailnet --slug as PENDING with a deny-all policy",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withPlatform(func(p *db.Platform) error {
			tn, err := p.CreateTailnet(clusterSlug)
			if err != nil {
				return err
			}

			return printJSON(map[string]any{"id": tn.ID, "slug": tn.Slug, "status": tn.Status, "cluster": tn.OwnerClusterID})
		})
	},
}

var tailnetStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Set the status of tailnet --slug",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withPlatform(func(p *db.Platform) error {
			tn, err := p.TailnetBySlug(clusterSlug)
			if err != nil {
				return err
			}

			id, err := tailnet.ParseID(tn.ID)
			if err != nil {
				return err
			}

			return p.SetTailnetStatus(id, types.TailnetStatus(clusterStatus))
		})
	},
}

var tailnetPolicyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Store the policy of ACTIVE tailnet --slug",
	RunE: func(_ *cobra.Command, _ []string) error {
		data, err := os.ReadFile(clusterPolicyFile)
		if err != nil {
			return err
		}

		return withPlatform(func(p *db.Platform) error {
			tn, err := p.TailnetBySlug(clusterSlug)
			if err != nil {
				return err
			}

			id, err := tailnet.ParseID(tn.ID)
			if err != nil {
				return err
			}

			hs, err := p.TenantDB(id)
			if err != nil {
				return err
			}

			_, err = hs.SetPolicy(string(data))
			if err != nil {
				return err
			}

			// A running cluster reloads the policy when the revision moves.
			return p.BumpPolicyRevision(id)
		})
	},
}

var tailnetAuthKeyCmd = &cobra.Command{
	Use:   "authkey",
	Short: "Mint a pre-auth key in ACTIVE tailnet --slug (secret on stdout only)",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterUser == "" {
			return errors.New("--user is required")
		}

		return withTenant(func(_ tailnet.ID, hs *db.HSDatabase) error {
			user, err := hs.GetUserByName(clusterUser)
			if err != nil {
				user, err = hs.CreateUser(types.User{Name: clusterUser})
				if err != nil {
					return fmt.Errorf("creating user %q: %w", clusterUser, err)
				}
			}

			uid := types.UserID(user.ID)
			exp := time.Now().Add(clusterKeyExpiry)

			pak, err := hs.CreatePreAuthKey(&uid, clusterKeyReusable, clusterKeyEphemeral, &exp, nil)
			if err != nil {
				return err
			}

			return printJSON(map[string]any{"key": pak.Key, "user": user.Name, "expiration": exp.UTC()})
		})
	},
}

var tailnetNodesCmd = &cobra.Command{
	Use:   "nodes",
	Short: "List the nodes of ACTIVE tailnet --slug",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withTenant(func(id tailnet.ID, hs *db.HSDatabase) error {
			nodes, err := hs.ListNodes()
			if err != nil {
				return err
			}

			out := make([]map[string]any, 0, len(nodes))
			for _, n := range nodes {
				var v4, v6 string
				if n.IPv4 != nil {
					v4 = n.IPv4.String()
				}

				if n.IPv6 != nil {
					v6 = n.IPv6.String()
				}

				out = append(out, map[string]any{
					"id": n.ID, "tailnet": id.String(), "hostname": n.Hostname, "given_name": n.GivenName,
					"ipv4": v4, "ipv6": v6,
					"machine_key": n.MachineKey.ShortString(), "node_key": n.NodeKey.ShortString(),
				})
			}

			return printJSON(out)
		})
	},
}

var tailnetReassignCmd = &cobra.Command{
	Use:   "reassign",
	Short: "Move tailnet --slug to cluster --to at the next owner_epoch (compare-and-swap)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withPlatform(func(p *db.Platform) error {
			tn, err := p.TailnetBySlug(clusterSlug)
			if err != nil {
				return err
			}

			id, err := tailnet.ParseID(tn.ID)
			if err != nil {
				return err
			}

			epoch, err := p.ReassignTailnet(id, tn.OwnerEpoch, clusterMoveTo)
			if err != nil {
				return err
			}

			return printJSON(map[string]any{"id": tn.ID, "slug": tn.Slug, "owner": clusterMoveTo, "epoch": epoch})
		})
	},
}
