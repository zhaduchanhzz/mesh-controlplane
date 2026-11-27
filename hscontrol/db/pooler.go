package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

var (
	// ErrPoolerMisconfigured reports a connection pooler (PgBouncer) set up
	// in a way this code cannot work with (ADR 0006 §3.5): the process
	// refuses to start rather than fail at random under load.
	ErrPoolerMisconfigured = errors.New("database connection pooler is misconfigured for mesh-controlplane")

	// ErrReadOnlyDatabase reports that a process that writes reached only
	// read-only servers: a replica, or a pooler in front of one.
	ErrReadOnlyDatabase = errors.New("database server is read-only")
)

const (
	// poolerCheckQuery has a parameter, so with pgx's default exec mode
	// (cache_statement) every connection prepares it under the same name.
	poolerCheckQuery = `SELECT $1::int + 1`
	// poolerReadOnlyQuery is what pgx's target_session_attrs=read-write
	// asks too.
	poolerReadOnlyQuery = `SHOW transaction_read_only`

	poolerCheckTimeout = 15 * time.Second
	poolerCheckRounds  = 3
)

// checkPooler is the start-up self-check of a PostgreSQL configuration
// (P13b, ADR 0006 §3.5), run before anything else reads the database:
//
//   - two connections of their own run the same parameterized statement in
//     turn, each run a transaction of its own. Behind PgBouncer in
//     transaction mode they share server connections, so without
//     prepared statement support (max_prepared_statements = 0) the second
//     prepare of the same name fails ("already exists") or a run finds it
//     missing ("does not exist"). Without the check that happens only
//     under load, at random: one process alone usually keeps one server
//     connection;
//   - a startup parameter the pooler refuses (statement_timeout without
//     track_extra_parameters) fails the connection itself;
//   - with requireWritable, SHOW transaction_read_only must be off.
//
// Every failure is ErrPoolerMisconfigured or ErrReadOnlyDatabase with what
// to change. The connections are closed before it returns. SQLite passes.
func checkPooler(cfg types.DatabaseConfig, requireWritable bool) error {
	if cfg.Type != types.DatabasePostgres {
		return nil
	}

	pg := cfg.Postgres

	pass, err := types.ResolveSecret("database.postgres.pass", string(pg.Pass))
	if err != nil {
		return err
	}

	pg.Pass = types.Secret(pass)

	// "pgx" is registered by gorm's PostgreSQL driver (pgx/v5/stdlib).
	sqlDB, err := sql.Open("pgx", pg.DSN())
	if err != nil {
		return fmt.Errorf("pooler self-check: %w", err)
	}
	defer sqlDB.Close()

	sqlDB.SetMaxOpenConns(2)
	sqlDB.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), poolerCheckTimeout)
	defer cancel()

	where := poolerServers(cfg.Postgres)

	var conns [2]*sql.Conn

	defer func() {
		for _, c := range conns {
			if c != nil {
				_ = c.Close()
			}
		}
	}()

	for i := range conns {
		conns[i], err = sqlDB.Conn(ctx)
		if err != nil {
			return classifyPoolerError(where, err)
		}
	}

	for round := range poolerCheckRounds {
		for _, c := range conns {
			var n int

			err = c.QueryRowContext(ctx, poolerCheckQuery, round).Scan(&n)
			if err != nil {
				return classifyPoolerError(where, err)
			}

			if n != round+1 {
				return fmt.Errorf("%w: %s answered %d to %q with %d", ErrPoolerMisconfigured, where, n, poolerCheckQuery, round)
			}
		}
	}

	if !requireWritable {
		return nil
	}

	for _, c := range conns {
		var ro string

		err = c.QueryRowContext(ctx, poolerReadOnlyQuery).Scan(&ro)
		if err != nil {
			return classifyPoolerError(where, err)
		}

		if ro != "off" {
			return fmt.Errorf("%w: %s (transaction_read_only = %s): a replica, or a pooler in front of one; "+
				"list the primary's pooler first, or let database.postgres.target_session_attrs default to read-write",
				ErrReadOnlyDatabase, where, ro)
		}
	}

	return nil
}

// classifyPoolerError names the configuration behind a self-check error; an
// error it does not recognise (server down, wrong password) is returned as
// it is, wrapped.
func classifyPoolerError(where string, err error) error {
	msg := err.Error()

	switch {
	case strings.Contains(msg, "prepared statement"):
		return fmt.Errorf("%w: %s does not keep prepared statements across transactions "+
			"(PgBouncer in transaction mode with max_prepared_statements = 0?): set max_prepared_statements "+
			"(ADR 0006: 200), or database.postgres.query_exec_mode: simple_protocol: %w",
			ErrPoolerMisconfigured, where, err)
	case strings.Contains(msg, "unsupported startup parameter"):
		return fmt.Errorf("%w: %s refuses a startup parameter (PgBouncer: list it in track_extra_parameters, "+
			"or set it on the role with ALTER ROLE ... SET and remove it from database.postgres): %w",
			ErrPoolerMisconfigured, where, err)
	case strings.Contains(msg, "read only connection"):
		return fmt.Errorf("%w: every server of %s answers read-only (target_session_attrs read-write): "+
			"is the primary or its pooler listed and up?: %w", ErrReadOnlyDatabase, where, err)
	default:
		return fmt.Errorf("pooler self-check on %s: %w", where, err)
	}
}

// poolerServers names the servers of pg for messages, never the password.
func poolerServers(pg types.PostgresConfig) string {
	var hosts string

	switch list := pg.HostList(); {
	case len(list) > 0:
		hosts = strings.Join(list, ",")
	case pg.Port != 0:
		hosts = fmt.Sprintf("%s:%d", pg.Host, pg.Port)
	default:
		hosts = pg.Host
	}

	return fmt.Sprintf("database %q on %s", pg.Name, hosts)
}
