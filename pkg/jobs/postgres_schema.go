package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const postgresReportProbeInterval = 30 * time.Second

func isPgInsufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// probeReportColumnsWith checks pg_attribute for both job_kind and report_json on s.jobsTable
// using the caller's query runner (pool or active tx) so probing inside a transaction never
// acquires a second pool connection.
func (s *PostgresStore) probeReportColumnsWith(ctx context.Context, q pgQueryRower) (bool, error) {
	var colCount int
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_attribute
		WHERE attrelid = to_regclass($1)
		  AND attname IN ('job_kind', 'report_json')
		  AND NOT attisdropped`, s.jobsTable).Scan(&colCount)
	if err != nil {
		return false, err
	}
	supported := colCount == 2
	s.supportsReportStorage.Store(supported)
	s.lastReportProbeNs.Store(time.Now().UnixNano())
	return supported, nil
}

// probeReportColumns checks pg_attribute for both job_kind and report_json on s.jobsTable.
func (s *PostgresStore) probeReportColumns(ctx context.Context) (bool, error) {
	return s.probeReportColumnsWith(ctx, s.pool)
}

// tryMigrateReportColumns attempts idempotent ADD COLUMN IF NOT EXISTS on an existing jobs table.
// Under least-privilege DML-only roles (PostgreSQL 42501), it cleanly records supportsReportStorage=false
// without failing ensureSchema so Dashboard Studio continues operating uninterrupted.
func (s *PostgresStore) tryMigrateReportColumns(ctx context.Context) error {
	supported, err := s.probeReportColumns(ctx)
	if err != nil {
		return err
	}
	if supported {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.schemaLockKey()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s
		ADD COLUMN IF NOT EXISTS job_kind TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS report_json TEXT NOT NULL DEFAULT ''`, s.jobsTable))
	if err != nil {
		if isPgInsufficientPrivilege(err) {
			// Least-privilege DML-only role: leave supportsReportStorage=false and keep Studio working.
			s.supportsReportStorage.Store(false)
			s.lastReportProbeNs.Store(time.Now().UnixNano())
			return nil
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.supportsReportStorage.Store(true)
	s.lastReportProbeNs.Store(time.Now().UnixNano())
	return nil
}

// ensureReportStorageReady checks whether job_kind and report_json exist, performing a
// rate-limited re-probe so a DBA migration applied after startup takes effect without restart.
func (s *PostgresStore) ensureReportStorageReady(ctx context.Context) error {
	if s.supportsReportStorage.Load() {
		return nil
	}
	last := s.lastReportProbeNs.Load()
	if last == 0 || time.Since(time.Unix(0, last)) >= postgresReportProbeInterval {
		supported, err := s.probeReportColumns(ctx)
		if err != nil {
			return wrapPostgresErr("probeReportColumns", err)
		}
		if supported {
			return nil
		}
	}
	return fmt.Errorf("%w: run ALTER TABLE %s ADD COLUMN IF NOT EXISTS job_kind TEXT NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS report_json TEXT NOT NULL DEFAULT ''", ErrReportStorageMigrationRequired, s.jobsTable)
}

// ensureSchema idempotently initializes the plugin tables and indexes inside a serialized
// transaction so concurrent multi-replica startups never race on CREATE TABLE IF NOT EXISTS.
// If both tables already exist (e.g. pre-created by a DBA for a least-privilege DML-only role),
// ensureSchema probes report columns safely without failing on 42501 permission denial.
func (s *PostgresStore) ensureSchema(ctx context.Context) error {
	if s.schemaReady.Load() {
		return nil
	}

	select {
	case s.schemaSem <- struct{}{}:
		defer func() { <-s.schemaSem }()
	case <-ctx.Done():
		return wrapPostgresErr("ensureSchema", ctx.Err())
	}

	if s.schemaReady.Load() {
		return nil
	}

	// Fast existence check: allows least-privilege DML-only DB roles to operate without
	// CREATE privilege on the schema or table ownership once tables are pre-created.
	var bothExist bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL`,
		s.jobsTable, s.slotsTable).Scan(&bothExist); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	if bothExist {
		if err := s.tryMigrateReportColumns(ctx); err != nil {
			return wrapPostgresErr("ensureSchema", err)
		}
		s.schemaReady.Store(true)
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	defer rollbackTx(ctx, tx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, s.schemaLockKey()); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}

	// Re-check inside lock in case another replica just created the tables.
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL`,
		s.jobsTable, s.slotsTable).Scan(&bothExist); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	if bothExist {
		_ = tx.Commit(ctx)
		if err := s.tryMigrateReportColumns(ctx); err != nil {
			return wrapPostgresErr("ensureSchema", err)
		}
		s.schemaReady.Store(true)
		return nil
	}

	if s.schema != DefaultPostgresSchema {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, s.schema).Scan(&exists); err != nil {
			return wrapPostgresErr("ensureSchema", err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{s.schema}.Sanitize()); err != nil {
				return wrapPostgresErr("ensureSchema", err)
			}
		}
	}

	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			job_id TEXT PRIMARY KEY,
			org_id BIGINT NOT NULL,
			user_login TEXT NOT NULL,
			owner_key TEXT NOT NULL,
			datasource_uid TEXT NOT NULL,
			query_kind TEXT NOT NULL DEFAULT '',
			scope_json TEXT NOT NULL DEFAULT '',
			import_source_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			current_ir_json TEXT NOT NULL DEFAULT '',
			current_rev INTEGER NOT NULL DEFAULT 0,
			history_json TEXT NOT NULL DEFAULT '[]',
			history_len INTEGER NOT NULL DEFAULT 0,
			last_diff_json TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			warnings_json TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			repair_attempts_used INTEGER NOT NULL DEFAULT 0,
			max_repair_attempts INTEGER NOT NULL DEFAULT 0,
			llm_meta_json TEXT NOT NULL DEFAULT '',
			known_gauges_json TEXT NOT NULL DEFAULT '[]',
			created_at_ms BIGINT NOT NULL,
			updated_at_ms BIGINT NOT NULL,
			expires_at_ms BIGINT NOT NULL,
			active_op_token BIGINT NOT NULL DEFAULT 0,
			active_op_until_ms BIGINT NOT NULL DEFAULT 0,
			job_kind TEXT NOT NULL DEFAULT '',
			report_json TEXT NOT NULL DEFAULT ''
		)`, s.jobsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (org_id, updated_at_ms, job_id)`,
			pgx.Identifier{s.tablePrefix + "jobs_org_updated_idx"}.Sanitize(), s.jobsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (expires_at_ms)`,
			pgx.Identifier{s.tablePrefix + "jobs_expires_idx"}.Sanitize(), s.jobsTable),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			org_id BIGINT NOT NULL,
			slot_id TEXT NOT NULL,
			owner_key TEXT NOT NULL,
			expires_at_ms BIGINT NOT NULL,
			PRIMARY KEY (org_id, slot_id)
		)`, s.slotsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (org_id, expires_at_ms)`,
			pgx.Identifier{s.tablePrefix + "slots_org_exp_idx"}.Sanitize(), s.slotsTable),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (expires_at_ms)`,
			pgx.Identifier{s.tablePrefix + "slots_exp_idx"}.Sanitize(), s.slotsTable),
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return wrapPostgresErr("ensureSchema", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return wrapPostgresErr("ensureSchema", err)
	}
	s.supportsReportStorage.Store(true)
	s.lastReportProbeNs.Store(time.Now().UnixNano())
	s.schemaReady.Store(true)
	return nil
}
