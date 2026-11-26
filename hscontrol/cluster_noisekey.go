package hscontrol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"tailscale.com/types/key"
)

// ErrNoiseKeyMissing reports a cluster process without its Noise key file
// (P13e, ADR 0014 §3). A cluster process never creates a key on its own:
// every process of a cluster serves with the one key the registry names,
// so a new key would only be refused at the lease, silently.
var ErrNoiseKeyMissing = errors.New("cluster Noise private key file is missing")

// ClusterOption changes how [NewCluster] builds a cluster process.
type ClusterOption func(*clusterOptions)

type clusterOptions struct {
	initNoiseKey bool
}

// WithNoiseKeyInit lets NewCluster create the cluster's Noise key when its
// file is missing and the registry names no key for the cluster yet: the
// first process of a new cluster (`headscale cluster serve
// --init-noise-key`). With a key in the registry it still refuses.
func WithNoiseKeyInit() ClusterOption {
	return func(o *clusterOptions) { o.initNoiseKey = true }
}

// readClusterNoiseKey reads the cluster's Noise key at path. A missing
// file is (nil, nil) only with init; otherwise ErrNoiseKeyMissing.
func readClusterNoiseKey(path string, init bool) (*key.MachinePrivate, error) {
	k, err := config.ReadPrivateKey(path)

	switch {
	case err == nil:
		return k, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("reading the cluster Noise private key: %w", err)
	case !init:
		return nil, fmt.Errorf("%w: %s; copy the key file of this cluster from its other process "+
			"(or, for the first process of a new cluster, start it once with --init-noise-key)", ErrNoiseKeyMissing, path)
	default:
		return nil, nil //nolint:nilnil // missing, and the caller may create it
	}
}

// initClusterNoiseKey creates the cluster's Noise key at path, only when
// the registry names no key for the cluster (a new cluster).
func initClusterNoiseKey(platform *db.Platform, path string) (*key.MachinePrivate, error) {
	registered, err := platform.RegisteredNoiseKey()
	if err != nil {
		return nil, err
	}

	if registered != "" {
		return nil, fmt.Errorf("%w: %s, and the registry names key %s for cluster %s: "+
			"copy that key's file from the cluster's other process, or rotate (docs/ops/noise-key-rotation.md)",
			ErrNoiseKeyMissing, path, registered, platform.ClusterID())
	}

	k := key.NewMachine()

	err = WriteNoiseKeyFile(path, k)
	if err != nil {
		return nil, err
	}

	log.Warn().Str("path", path).Str("public_key", k.Public().String()).
		Msg("created the cluster Noise key (--init-noise-key); copy this file to the cluster's other processes")

	return &k, nil
}

// WriteNoiseKeyFile writes k to path atomically, mode 0600: a temporary
// file in the same directory, synced, renamed over path, then the
// directory synced. A reader sees the old file or the new one, never a
// partial key.
func WriteNoiseKeyFile(path string, k key.MachinePrivate) (retErr error) {
	text, err := k.MarshalText()
	if err != nil {
		return fmt.Errorf("encoding the Noise private key: %w", err)
	}

	dir := filepath.Dir(path)

	err = util.EnsureDir(dir)
	if err != nil {
		return fmt.Errorf("ensuring the Noise key directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the Noise private key: %w", err)
	}

	defer func() {
		if retErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	// CreateTemp makes it 0600 already; said again for the reader.
	err = tmp.Chmod(privateKeyFileMode)
	if err != nil {
		return fmt.Errorf("writing the Noise private key: %w", err)
	}

	_, err = tmp.Write(append(text, '\n'))
	if err != nil {
		return fmt.Errorf("writing the Noise private key: %w", err)
	}

	err = tmp.Sync()
	if err != nil {
		return fmt.Errorf("writing the Noise private key: %w", err)
	}

	err = tmp.Close()
	if err != nil {
		return fmt.Errorf("writing the Noise private key: %w", err)
	}

	err = os.Rename(tmp.Name(), path)
	if err != nil {
		return fmt.Errorf("installing the Noise private key at %s: %w", path, err)
	}

	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync() // best effort: some filesystems refuse a directory sync
		_ = d.Close()
	}

	return nil
}
