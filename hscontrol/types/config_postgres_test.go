package types

import (
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P13b (ADR 0011 §D): several servers become pgx's multi-host form (host
// and port lists as parameters), with target_session_attrs, a per-host
// connect timeout and statement_timeout. That pgx reads them as intended is
// tested against real servers in hscontrol/db (pooler_test.go).
func TestPostgresConfigMultiHostDSN(t *testing.T) {
	tests := []struct {
		name  string
		cfg   PostgresConfig
		uhost string // the URL's own host
		hosts []string
		want  map[string]string
	}{
		{
			name:  "hosts with their own ports",
			cfg:   PostgresConfig{Hosts: []string{"pg-01:6432", "pg-02:6432"}, Name: "mesh", User: "hs"},
			hosts: []string{"pg-01:6432", "pg-02:6432"},
			want:  map[string]string{"host": "pg-01,pg-02", "port": "6432,6432"},
		},
		{
			name:  "an entry without a port takes database.postgres.port; IPv6 bare or bracketed",
			cfg:   PostgresConfig{Hosts: []string{"pg-01", "192.0.2.8:7432", "2001:db8::1", "[2001:db8::2]:6433", "[2001:db8::3]"}, Port: 6432, Name: "mesh"},
			hosts: []string{"pg-01:6432", "192.0.2.8:7432", "[2001:db8::1]:6432", "[2001:db8::2]:6433", "[2001:db8::3]:6432"},
			want: map[string]string{
				"host": "pg-01,192.0.2.8,2001:db8::1,2001:db8::2,2001:db8::3",
				"port": "6432,7432,6432,6433,6432",
			},
		},
		{
			name:  "no port anywhere: 5432, one port per host",
			cfg:   PostgresConfig{Hosts: []string{"a", "b:6432"}, Name: "mesh"},
			hosts: []string{"a:5432", "b:6432"},
			want:  map[string]string{"host": "a,b", "port": "5432,6432"},
		},
		{
			name:  "host may hold the libpq list form",
			cfg:   PostgresConfig{Host: "pg-01:6432, pg-02:6432", Name: "mesh"},
			hosts: []string{"pg-01:6432", "pg-02:6432"},
			want:  map[string]string{"host": "pg-01,pg-02", "port": "6432,6432"},
		},
		{
			name: "target_session_attrs, connect_timeout and statement_timeout are passed to pgx",
			cfg: PostgresConfig{
				Hosts: []string{"a:1", "b:2"}, Name: "mesh", Ssl: "false",
				TargetSessionAttrs: PostgresTargetReadWrite, ConnectTimeoutSecs: 2, StatementTimeout: 1500 * time.Millisecond,
			},
			hosts: []string{"a:1", "b:2"},
			want: map[string]string{
				"host": "a,b", "port": "1,2",
				"target_session_attrs": "read-write", "connect_timeout": "2", "statement_timeout": "1500", "sslmode": "disable",
			},
		},
		{
			name:  "a single host keeps its old form",
			cfg:   PostgresConfig{Host: "db", Port: 5432, Name: "mesh", ConnectTimeoutSecs: 2},
			uhost: "db:5432",
			want:  map[string]string{"connect_timeout": "2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.hosts, tt.cfg.HostList())

			u, err := url.Parse(tt.cfg.DSN())
			require.NoError(t, err)

			assert.Equal(t, tt.uhost, u.Host)
			assert.Equal(t, "/mesh", u.Path)

			query := u.Query()
			assert.Len(t, query, len(tt.want), "%v", query)

			for k, v := range tt.want {
				assert.Equal(t, v, query.Get(k), k)
			}
		})
	}
}
