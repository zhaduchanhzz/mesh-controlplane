package cli

import (
	"context"
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
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
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
	clusterRegClass     string
	clusterRegMax       int64
	clusterRegEndpoint  string
	clusterKeyID        uint64
)

var (
	errClusterIDRequired = errors.New("--cluster-id is required")
	errPolicyFlagChoice  = errors.New("give exactly one of --file, --clear, --show")
	errUserRequired      = errors.New("--user is required")
	errKeyIDRequired     = errors.New("--id is required")
)

func init() {
	clusterCmd.PersistentFlags().StringVar(&clusterID, "cluster-id", "", "ID of the cluster this process serves (required)")

	tailnetCmd.PersistentFlags().StringVar(&clusterSlug, "slug", "", "tailnet slug")

	tailnetStatusCmd.Flags().StringVar(&clusterStatus, "set", "", "new status: PENDING, ACTIVE, SUSPENDED or DELETING")
	tailnetPolicyCmd.Flags().StringVar(&clusterPolicyFile, "file", "", "policy file (HuJSON)")
	tailnetDNSCmd.Flags().StringVar(&clusterDNSFile, "file", "", "DNS settings file (JSON; see ADR 0008)")
	tailnetDNSCmd.Flags().BoolVar(&clusterDNSClear, "clear", false, "drop the tailnet's DNS settings (the defaults)")
	tailnetDNSCmd.Flags().BoolVar(&clusterDNSShow, "show", false, "print the stored DNS settings")
	tailnetAuthKeyCmd.Flags().StringVar(&clusterUser, "user", "", "user owning the nodes (created if missing)")
	tailnetAuthKeyCmd.Flags().DurationVar(&clusterKeyExpiry, "expiration", time.Hour, "key lifetime")
	tailnetAuthKeyCmd.Flags().BoolVar(&clusterKeyReusable, "reusable", false, "key can register several nodes")
	tailnetAuthKeyCmd.Flags().BoolVar(&clusterKeyEphemeral, "ephemeral", false, "nodes are ephemeral")
	tailnetAuthKeyRevokeCmd.Flags().Uint64Var(&clusterKeyID, "id", 0, "ID of the pre-auth key (from `tailnet authkey`)")
	tailnetReassignCmd.Flags().StringVar(&clusterMoveTo, "to", "", "cluster that takes ownership (this cluster again = new epoch)")
	clusterRegisterCmd.Flags().StringVar(&clusterRegClass, "class", "shared", "shard class: shared or dedicated")
	clusterRegisterCmd.Flags().Int64Var(&clusterRegMax, "max-devices", 5000, "device budget of the cluster")
	clusterRegisterCmd.Flags().StringVar(&clusterRegEndpoint, "endpoint", "", "endpoint URL (default: server_url)")

	tailnetCmd.AddCommand(tailnetCreateCmd, tailnetStatusCmd, tailnetPolicyCmd, tailnetDNSCmd, tailnetAuthKeyCmd, tailnetAuthKeyRevokeCmd, tailnetNodesCmd, tailnetReassignCmd)
	clusterCmd.AddCommand(clusterServeCmd, clusterRegisterCmd, tailnetCmd)
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
	Use:   useCreate,
	Short: "Create tailnet --slug as PENDING with a deny-all policy",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withPlatform(func(p *db.Platform) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			tn, err := createTailnetCLI(ctx, p, clusterID, clusterSlug)
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
			return setTailnetPolicyCLI(p, clusterSlug, data, false)
		})
	},
}

// setTailnetPolicyCLI stores data as the policy of ACTIVE tailnet slug and
// bumps its policy revision.
func setTailnetPolicyCLI(p *db.Platform, slug string, data []byte, _ bool) error {
	tn, err := p.TailnetBySlug(slug)
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
}

var (
	clusterDNSFile  string
	clusterDNSClear bool
	clusterDNSShow  bool
)

// tailnetDNSCmd stores, clears or shows the DNS settings of tailnet --slug
// (P06, ADR 0008). The settings are validated before they are stored; a
// running cluster serves them to the tailnet's clients within seconds.
var tailnetDNSCmd = &cobra.Command{
	Use:   "dns",
	Short: "Store (--file), clear (--clear) or show (--show) the DNS settings of tailnet --slug",
	RunE: func(_ *cobra.Command, _ []string) error {
		chosen := 0

		for _, on := range []bool{clusterDNSFile != "", clusterDNSClear, clusterDNSShow} {
			if on {
				chosen++
			}
		}

		if chosen != 1 {
			return errPolicyFlagChoice
		}

		cfg, err := clusterConfig()
		if err != nil {
			return err
		}

		var settings *types.TenantDNS

		if clusterDNSFile != "" {
			data, err := os.ReadFile(clusterDNSFile)
			if err != nil {
				return err
			}

			settings, err = types.ParseTenantDNS(data)
			if err != nil {
				return err
			}
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

			if clusterDNSShow {
				d, err := p.TailnetDNS(id)
				if err != nil {
					return err
				}

				out, err := json.MarshalIndent(d, "", "  ")
				if err != nil {
					return err
				}

				fmt.Println(string(out)) //nolint:forbidigo // CLI output

				return nil
			}

			return p.SetTailnetDNS(id, settings, cfg.DNSConfig.BaseDomain, cfg.Tuning.TenantDNS)
		})
	},
}

var tailnetAuthKeyCmd = &cobra.Command{
	Use:   "authkey",
	Short: "Mint a pre-auth key in ACTIVE tailnet --slug (secret on stdout only)",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterUser == "" {
			return errUserRequired
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

			return printJSON(map[string]any{"id": pak.ID, "key": pak.Key, "user": user.Name, "expiration": exp.UTC()})
		})
	},
}

var tailnetAuthKeyRevokeCmd = &cobra.Command{
	Use:   "authkey-revoke",
	Short: "Revoke pre-auth key --id of ACTIVE tailnet --slug (nodes already registered with it stay)",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterKeyID == 0 {
			return errKeyIDRequired
		}

		// The tenant handle is scoped to --slug: a key ID of another tailnet
		// is not found here.
		return withTenant(func(_ tailnet.ID, hs *db.HSDatabase) error {
			return hs.RevokePreAuthKey(clusterKeyID)
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
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			tn, err := reassignTailnetCLI(ctx, p, clusterID, clusterSlug, clusterMoveTo)
			if err != nil {
				return err
			}

			// To another cluster this is a move request (ADR 0010): owner and
			// epoch change when the owner hands the tailnet over.
			return printJSON(map[string]any{
				"id": tn.ID, "slug": tn.Slug, "owner": tn.OwnerClusterID, "epoch": tn.OwnerEpoch,
				"moving_to": tn.MovingToClusterID,
			})
		})
	},
}

var clusterRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register --cluster-id in the placement registry as ACTIVE (operator)",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := clusterConfig()
		if err != nil {
			return err
		}

		endpoint := clusterRegEndpoint
		if endpoint == "" {
			endpoint = cfg.ServerURL
		}

		return withPlatform(func(p *db.Platform) error {
			reg, err := placement.New(p.RegistryDB(), placement.Config{})
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			c, err := reg.CreateCluster(ctx, cliOp(clusterID), placement.ClusterSpec{
				ID: clusterID, EndpointURL: endpoint, Class: placement.Class(clusterRegClass),
				State: placement.ClusterActive, MaxDevices: clusterRegMax,
			})
			if err != nil {
				return err
			}

			return printJSON(map[string]any{"id": c.ID, "class": c.Class, "state": c.State, "max_devices": c.MaxDevices})
		})
	},
}

// cliOp is the audit identity of the CLI of cluster (as `register` and
// `apikey` use).
func cliOp(cluster string) placement.Op { return placement.Op{Actor: "cli:" + cluster} }

// createTailnetCLI creates tailnet slug owned by cluster, PENDING with a
// deny-all policy, through the placement registry (P12, owner decision
// Q5): cluster must be registered and ACTIVE with room, a dedicated one
// with no other tailnet, and its process must hold the cluster lease
// (owner decision 2026-11-25): start the cluster before creating tailnets.
func createTailnetCLI(ctx context.Context, p *db.Platform, cluster, slug string) (types.Tailnet, error) {
	reg, err := placement.New(p.RegistryDB(), placement.Config{})
	if err != nil {
		return types.Tailnet{}, err
	}

	return reg.CreateTailnetOn(ctx, cliOp(cluster), slug, cluster)
}

// reassignTailnetCLI moves tailnet slug from cluster (its owner) to at
// the next owner_epoch through the registry's MoveOwner (Q5): to must be
// registered and ACTIVE, hold a live lease, with room for the tailnet's
// devices; to == cluster is a new epoch on the same cluster (no lease
// check).
func reassignTailnetCLI(ctx context.Context, p *db.Platform, cluster, slug, to string) (types.Tailnet, error) {
	tn, err := p.TailnetBySlug(slug) //nolint:contextcheck // Platform bounds its own lookups (lookupContext)
	if err != nil {
		return types.Tailnet{}, err
	}

	reg, err := placement.New(p.RegistryDB(), placement.Config{})
	if err != nil {
		return types.Tailnet{}, err
	}

	return reg.MoveOwner(ctx, cliOp(cluster), tn.ID, cluster, tn.OwnerEpoch, to)
}
