//go:build benchdb

// Command bench-db runs the shared-PostgreSQL benchmarks of tickets P14 (B2)
// and P12b (B3) against a real PostgreSQL, optionally behind PgBouncer.
//
// It is a benchmark harness, not production code: it is excluded from normal
// builds and from `go test ./...` by the benchdb build tag. Build it with
//
//	go build -tags benchdb -o bench-db ./cmd/bench-db
//
// Every tenant-data statement goes through the real v0.1 code paths:
// db.NewHeadscaleDatabase (schema_postgres.sql, version check, tenantscope
// plugin, ownership check of the runtime tailnet) and the exported
// HSDatabase write/read functions. Statements that the plan requires but
// that are not implemented yet (P10 owner/epoch fencing, P12b owner CAS)
// are written here as raw SQL and labelled "simulated" in the output.
//
// Subcommands:
//
//	bench-db b2      coordinator: spawns N active + N standby cluster processes
//	bench-db worker  one active cluster process (spawned by b2)
//	bench-db standby one standby cluster process (spawned by b2)
//	bench-db b3      tenant move: owner CAS vs per-row cluster_id rewrite
//
// Passwords are only read from files (-passfile); they are never printed.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

func main() {
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly})

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench-db b2|worker|standby|b3 [flags]")
		os.Exit(2)
	}

	var err error

	switch os.Args[1] {
	case "b2":
		err = runB2(os.Args[2:])
	case "worker":
		err = runWorker(os.Args[2:], false)
	case "standby":
		err = runWorker(os.Args[2:], true)
	case "b3":
		err = runB3(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "bench-db:", err)
		os.Exit(1)
	}
}

// pgTarget names one PostgreSQL endpoint (PostgreSQL itself or PgBouncer).
type pgTarget struct {
	Host     string
	Port     int
	DB       string
	User     string
	PassFile string
}

// config builds the runtime configuration the real binary would use.
func (t pgTarget) config(cluster, tailnetID string, maxOpen, maxIdle int) *types.Config {
	return &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host:                t.Host,
				Port:                t.Port,
				Name:                t.DB,
				User:                t.User,
				Pass:                types.Secret("file:" + t.PassFile),
				Ssl:                 "disable",
				MaxOpenConnections:  maxOpen,
				MaxIdleConnections:  maxIdle,
				ConnMaxIdleTimeSecs: 600,
			},
		},
		Policy:    types.PolicyConfig{Mode: types.PolicyModeDB},
		TailnetID: tailnetID,
		ClusterID: cluster,
	}
}

// dsn returns a DSN with the password resolved, for the platform (admin)
// connection of the coordinator. Never print it.
func (t pgTarget) dsn() (string, error) {
	pass, err := types.Secret("file:" + t.PassFile).Resolve()
	if err != nil {
		return "", err
	}

	pc := types.PostgresConfig{Host: t.Host, Port: t.Port, Name: t.DB, User: t.User, Pass: types.Secret(pass), Ssl: "disable"}

	return pc.DSN(), nil
}
