package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestPostgresHostsConfig (P13b, ADR 0011 §D): database.postgres.hosts,
// target_session_attrs, connect_timeout_secs, statement_timeout and the
// lease pool keys load with their defaults, and an unusable host list is
// refused at load.
func TestPostgresHostsConfig(t *testing.T) {
	base := `---
noise:
  private_key_path: noise_private.key
server_url: http://127.0.0.1:8080
dns:
  override_local_dns: false
database:
  type: postgres
  postgres:
    name: headscale
    user: headscale
`

	tests := []struct {
		name    string
		extra   string
		check   func(t *testing.T, pg types.PostgresConfig)
		wantErr string
	}{
		{
			name:  "defaults",
			extra: "    host: db\n",
			check: func(t *testing.T, pg types.PostgresConfig) {
				t.Helper()
				assert.Empty(t, pg.Hosts)
				assert.Equal(t, types.PostgresTargetSessionAttrs(""), pg.TargetSessionAttrs, "unset: read-write where headscale writes")
				assert.Equal(t, 2, pg.ConnectTimeoutSecs)
				assert.Zero(t, pg.StatementTimeout)
				assert.Empty(t, pg.LeaseName)
				assert.Equal(t, 2, pg.LeaseMaxOpenConnections)
			},
		},
		{
			name: "two PgBouncers, lease alias",
			extra: "    hosts: [\"pg-01:6432\", \"pg-02:6432\"]\n    target_session_attrs: read-write\n" +
				"    connect_timeout_secs: 3\n    statement_timeout: 30s\n    lease_name: mesh_lease\n    lease_max_open_conns: 4\n",
			check: func(t *testing.T, pg types.PostgresConfig) {
				t.Helper()
				assert.Equal(t, []string{"pg-01:6432", "pg-02:6432"}, pg.HostList())
				assert.Equal(t, types.PostgresTargetReadWrite, pg.TargetSessionAttrs)
				assert.Equal(t, 3, pg.ConnectTimeoutSecs)
				assert.Equal(t, 30*time.Second, pg.StatementTimeout)
				assert.Equal(t, "mesh_lease", pg.LeaseName)
				assert.Equal(t, 4, pg.LeaseMaxOpenConnections)
			},
		},
		{
			name:    "host and hosts",
			extra:   "    host: db\n    hosts: [\"pg-01:6432\"]\n",
			wantErr: "database.postgres.host and database.postgres.hosts are mutually exclusive",
		},
		{
			name:    "unknown target_session_attrs",
			extra:   "    host: db\n    target_session_attrs: master\n",
			wantErr: "database.postgres.target_session_attrs has an unsupported value",
		},
		{
			name:    "socket in a list",
			extra:   "    host: \"/run/postgresql,pg-02:6432\"\n",
			wantErr: "host list entry is empty or a Unix socket",
		},
		{
			name:    "bad port",
			extra:   "    hosts: [\"pg-01:0\"]\n",
			wantErr: "host list entry has an invalid port",
		},
		{
			name:    "negative connect timeout",
			extra:   "    host: db\n    connect_timeout_secs: -1\n",
			wantErr: "database.postgres.connect_timeout_secs is negative",
		},
		{
			name:    "sub-millisecond statement timeout",
			extra:   "    host: db\n    statement_timeout: 500us\n",
			wantErr: "database.postgres.statement_timeout is below one millisecond",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(base+tt.extra), 0o600))

			cfg, err := loadServer(dir, false)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			tt.check(t, cfg.Database.Postgres)
		})
	}
}
