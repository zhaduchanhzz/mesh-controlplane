package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"tailscale.com/types/key"
)

// `headscale cluster noise-key` rotates the cluster's Noise key (P13e, ADR
// 0014; runbook docs/ops/noise-key-rotation.md). The key model does not
// change: one key per cluster, no overlap of two keys (AGENTS rule 12).
// The new key reaches clients by a switchover: `install` puts the private
// key in place on a stopped process, `rotate` names its public key in the
// registry, and the active process, still on the old key, then stops
// serving within one reconcile tick and gives the lease back to a process
// holding the new key. Private keys are never printed.

var (
	noiseKeyFrom      string
	noiseKeyGenerate  bool
	noiseKeyPublicKey string

	errNoiseKeySource = errors.New("give exactly one of --from <file> and --generate")
	errNoiseKeySame   = errors.New("--from names noise.private_key_path itself")
)

func init() {
	noiseKeyInstallCmd.Flags().StringVar(&noiseKeyFrom, "from", "",
		"private key file to install (from `headscale generate private-key`); delete it afterwards")
	noiseKeyInstallCmd.Flags().BoolVar(&noiseKeyGenerate, "generate", false, "generate the new private key here")
	noiseKeyRotateCmd.Flags().StringVar(&noiseKeyPublicKey, "public-key", "",
		"public key to register (mkey:...); default: the public key of noise.private_key_path")

	noiseKeyCmd.AddCommand(noiseKeyShowCmd, noiseKeyInstallCmd, noiseKeyRotateCmd)
	clusterCmd.AddCommand(noiseKeyCmd)
}

var noiseKeyCmd = &cobra.Command{
	Use:   "noise-key",
	Short: "Show, install and rotate the cluster's Noise key (ADR 0014)",
}

// noiseKeyState is what `noise-key show` prints: public keys only.
type noiseKeyState struct {
	Cluster             string `json:"cluster"`
	File                string `json:"file"`
	FilePublicKey       string `json:"file_public_key,omitempty"`
	FileError           string `json:"file_error,omitempty"`
	RegisteredPublicKey string `json:"registered_public_key,omitempty"`
	Match               bool   `json:"match"`
}

// noiseKeyStatus compares the key file at path with the registry. A file
// that cannot be read is reported in FileError.
func noiseKeyStatus(p *db.Platform, path string) (noiseKeyState, error) {
	st := noiseKeyState{Cluster: p.ClusterID(), File: path}

	k, err := config.ReadPrivateKey(path)
	if err != nil {
		st.FileError = err.Error()
	} else {
		st.FilePublicKey = k.Public().String()
	}

	st.RegisteredPublicKey, err = p.RegisteredNoiseKey()
	if err != nil {
		return st, err
	}

	st.Match = st.FilePublicKey != "" && st.FilePublicKey == st.RegisteredPublicKey

	return st, nil
}

// installNoiseKey validates the new private key (from a file, or
// generated) and writes it atomically to target. It returns the new
// public key.
func installNoiseKey(target, from string, generate bool) (string, error) {
	if (from == "") == !generate {
		return "", errNoiseKeySource
	}

	var k key.MachinePrivate

	if generate {
		k = key.NewMachine()
	} else {
		absFrom, err := filepath.Abs(from)
		if err != nil {
			return "", err
		}

		absTarget, err := filepath.Abs(target)
		if err != nil {
			return "", err
		}

		if absFrom == absTarget {
			return "", errNoiseKeySame
		}

		read, err := config.ReadPrivateKey(from)
		if err != nil {
			return "", fmt.Errorf("--from %s: %w", from, err)
		}

		k = *read
	}

	err := hscontrol.WriteNoiseKeyFile(target, k)
	if err != nil {
		return "", err
	}

	return k.Public().String(), nil
}

// rotateNoiseKeyCLI names pub (default: the public key of the key file at
// path) as the cluster's Noise key in the registry, audited as the CLI of
// cluster.
func rotateNoiseKeyCLI(ctx context.Context, p *db.Platform, cluster, pub, path string) (placement.Cluster, error) {
	if pub == "" {
		k, err := config.ReadPrivateKey(path)
		if err != nil {
			return placement.Cluster{}, fmt.Errorf("no --public-key, and the key file: %w", err)
		}

		pub = k.Public().String()
	}

	reg, err := placement.New(p.RegistryDB(), placement.Config{})
	if err != nil {
		return placement.Cluster{}, err
	}

	return reg.RotateNoiseKey(ctx, cliOp(cluster), cluster, pub)
}

var noiseKeyShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Compare the public key of noise.private_key_path with the registered one",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := clusterConfig()
		if err != nil {
			return err
		}

		return withPlatform(func(p *db.Platform) error {
			st, err := noiseKeyStatus(p, cfg.AbsPath(cfg.Noise.PrivateKeyPath))
			if err != nil {
				return err
			}

			return printJSON(st)
		})
	},
}

var noiseKeyInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Write a new Noise private key to noise.private_key_path atomically (process stopped; no database change)",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := clusterConfig()
		if err != nil {
			return err
		}

		target := cfg.AbsPath(cfg.Noise.PrivateKeyPath)

		var previous string
		if k, err := config.ReadPrivateKey(target); err == nil { //nolint:noinlineerr
			previous = k.Public().String()
		}

		pub, err := installNoiseKey(target, noiseKeyFrom, noiseKeyGenerate)
		if err != nil {
			return err
		}

		return printJSON(map[string]any{"file": target, "public_key": pub, "replaced_public_key": previous})
	},
}

var noiseKeyRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Register the cluster's new Noise public key (audited); the active process on the old key steps down",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := clusterConfig()
		if err != nil {
			return err
		}

		return withPlatform(func(p *db.Platform) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			before, err := p.RegisteredNoiseKey() //nolint:contextcheck // Platform bounds its own lookups (lookupContext)
			if err != nil {
				return err
			}

			c, err := rotateNoiseKeyCLI(ctx, p, clusterID, noiseKeyPublicKey, cfg.AbsPath(cfg.Noise.PrivateKeyPath))
			if err != nil {
				return err
			}

			return printJSON(map[string]any{
				"cluster": c.ID, "from": before, "to": c.NoisePublicKey, "revision": c.Revision,
			})
		})
	},
}
