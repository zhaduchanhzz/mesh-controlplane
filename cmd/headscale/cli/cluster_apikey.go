package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
)

// Tenant API keys (P08b, owner decision Q3): the operator mints the key a
// tenant uses on the public /api/v2 of its cluster. Each key is bound to
// tailnet --slug, audited in platform_audit (actor cli:<cluster-id>), and
// printed once: it is not stored and cannot be shown again.

var (
	clusterAPIKeyExpiry time.Duration
	clusterAPIKeyID     uint64
	clusterAPIKeyUser   string
)

func init() {
	tailnetAPIKeyCreateCmd.Flags().DurationVar(&clusterAPIKeyExpiry, "expiration", 90*24*time.Hour,
		"key lifetime (at most 8760h)")
	tailnetAPIKeyCreateCmd.Flags().StringVar(&clusterAPIKeyUser, "user", "",
		"user of the tailnet the key acts as (its untagged pre-auth keys belong to that user); must exist")
	tailnetAPIKeyRevokeCmd.Flags().Uint64Var(&clusterAPIKeyID, "id", 0, "ID of the API key (from `tailnet apikey list`)")

	tailnetAPIKeyCmd.AddCommand(tailnetAPIKeyCreateCmd, tailnetAPIKeyListCmd, tailnetAPIKeyRevokeCmd)
	tailnetCmd.AddCommand(tailnetAPIKeyCmd)
}

var tailnetAPIKeyCmd = &cobra.Command{
	Use:   "apikey",
	Short: "API keys of tailnet --slug for its public /api/v2 (platform-issued, audited)",
}

// withRegistry runs fn with the platform registry and the ID of --slug.
func withRegistry(fn func(ctx context.Context, reg *placement.Registry, id string) error) error {
	return withPlatform(func(p *db.Platform) error {
		tn, err := p.TailnetBySlug(clusterSlug)
		if err != nil {
			return err
		}

		reg, err := placement.New(p.RegistryDB(), placement.Config{})
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) //nolint:mnd
		defer cancel()

		return fn(ctx, reg, tn.ID)
	})
}

var tailnetAPIKeyCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Mint an API key for tailnet --slug; the key is printed once",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withRegistry(func(ctx context.Context, reg *placement.Registry, id string) error {
			info, key, err := reg.CreateAPIKey(ctx, cliOp(clusterID), id, clusterAPIKeyExpiry, clusterAPIKeyUser)
			if err != nil {
				return err
			}

			return printJSON(struct {
				placement.APIKeyInfo

				Key string `json:"key"`
			}{info, key})
		})
	},
}

var tailnetAPIKeyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the API keys of tailnet --slug (no secrets)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return withRegistry(func(ctx context.Context, reg *placement.Registry, id string) error {
			keys, err := reg.ListAPIKeys(ctx, id)
			if err != nil {
				return err
			}

			return printJSON(keys)
		})
	},
}

var tailnetAPIKeyRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke API key --id of tailnet --slug",
	RunE: func(_ *cobra.Command, _ []string) error {
		if clusterAPIKeyID == 0 {
			return fmt.Errorf("%w: --id is required", errClusterLive)
		}

		return withRegistry(func(ctx context.Context, reg *placement.Registry, id string) error {
			info, err := reg.RevokeAPIKey(ctx, cliOp(clusterID), id, clusterAPIKeyID)
			if err != nil {
				return err
			}

			return printJSON(info)
		})
	},
}
