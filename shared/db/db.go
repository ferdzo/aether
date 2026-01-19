package db

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"aether/shared/protocol"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type DB struct {
	db *sql.DB
}

func NewDB(dbPath string) (*DB, error) {
	// Enable WAL mode and busy timeout for better concurrency
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// Limit connections to avoid contention
	db.SetMaxOpenConns(1)
	return &DB{db: db}, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) Migrate() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to read migrations: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		version := entry.Name()

		var exists int
		err := d.db.QueryRow("SELECT 1 FROM schema_migrations WHERE version = ?", version).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("failed to check migration %s: %w", version, err)
		}

		content, err := fs.ReadFile(migrationsFS, "migrations/"+version)
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", version, err)
		}

		if _, err := d.db.Exec(string(content)); err != nil {
			return fmt.Errorf("failed to apply migration %s: %w", version, err)
		}

		if _, err := d.db.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", version, time.Now()); err != nil {
			return fmt.Errorf("failed to record migration %s: %w", version, err)
		}
	}

	return nil
}

func (d *DB) CreateFunction(fn *protocol.FunctionMetadata) error {
	now := time.Now()
	fn.CreatedAt = now
	fn.UpdatedAt = now
	if fn.EnvVars == nil {
		fn.EnvVars = make(map[string]string)
	}
	if fn.Port == 0 {
		fn.Port = 3000
	}
	if fn.Entrypoint == "" {
		fn.Entrypoint = "handler.js"
	}
	envVarsJSON, err := json.Marshal(fn.EnvVars)
	if err != nil {
		return fmt.Errorf("failed to marshal env_vars: %w", err)
	}
	_, err = d.db.Exec(
		`INSERT INTO functions (id, name, runtime, entrypoint, code_path, vcpu, memory_mb, port, env_vars, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fn.ID, fn.Name, fn.Runtime, fn.Entrypoint, fn.CodePath, fn.VCPU, fn.MemoryMB, fn.Port, string(envVarsJSON), fn.CreatedAt, fn.UpdatedAt,
	)
	return err
}

func (d *DB) GetFunction(id string) (*protocol.FunctionMetadata, error) {
	row := d.db.QueryRow(
		`SELECT id, name, runtime, entrypoint, code_path, vcpu, memory_mb, port, env_vars, created_at, updated_at
		 FROM functions WHERE id = ?`, id,
	)
	var fn protocol.FunctionMetadata
	var envVarsJSON string
	var entrypoint sql.NullString
	err := row.Scan(&fn.ID, &fn.Name, &fn.Runtime, &entrypoint, &fn.CodePath, &fn.VCPU, &fn.MemoryMB, &fn.Port, &envVarsJSON, &fn.CreatedAt, &fn.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fn.Entrypoint = entrypoint.String
	if fn.Entrypoint == "" {
		fn.Entrypoint = "handler.js"
	}
	if err := json.Unmarshal([]byte(envVarsJSON), &fn.EnvVars); err != nil {
		return nil, fmt.Errorf("failed to unmarshal env_vars: %w", err)
	}
	return &fn, nil
}

func (d *DB) GetFunctions() ([]*protocol.FunctionMetadata, error) {
	rows, err := d.db.Query(
		`SELECT id, name, runtime, entrypoint, code_path, vcpu, memory_mb, port, env_vars, created_at, updated_at FROM functions`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	functions := make([]*protocol.FunctionMetadata, 0)
	for rows.Next() {
		var fn protocol.FunctionMetadata
		var envVarsJSON string
		var entrypoint sql.NullString
		err := rows.Scan(&fn.ID, &fn.Name, &fn.Runtime, &entrypoint, &fn.CodePath, &fn.VCPU, &fn.MemoryMB, &fn.Port, &envVarsJSON, &fn.CreatedAt, &fn.UpdatedAt)
		if err != nil {
			return nil, err
		}
		fn.Entrypoint = entrypoint.String
		if fn.Entrypoint == "" {
			fn.Entrypoint = "handler.js"
		}
		if err := json.Unmarshal([]byte(envVarsJSON), &fn.EnvVars); err != nil {
			return nil, fmt.Errorf("failed to unmarshal env_vars: %w", err)
		}
		functions = append(functions, &fn)
	}
	return functions, nil
}

func (d *DB) UpdateFunction(fn *protocol.FunctionMetadata) error {
	fn.UpdatedAt = time.Now()
	if fn.EnvVars == nil {
		fn.EnvVars = make(map[string]string)
	}
	if fn.Entrypoint == "" {
		fn.Entrypoint = "handler.js"
	}
	envVarsJSON, err := json.Marshal(fn.EnvVars)
	if err != nil {
		return fmt.Errorf("failed to marshal env_vars: %w", err)
	}
	_, err = d.db.Exec(
		`UPDATE functions SET name = ?, runtime = ?, entrypoint = ?, code_path = ?, vcpu = ?, memory_mb = ?, port = ?, env_vars = ?, updated_at = ?
		 WHERE id = ?`,
		fn.Name, fn.Runtime, fn.Entrypoint, fn.CodePath, fn.VCPU, fn.MemoryMB, fn.Port, string(envVarsJSON), fn.UpdatedAt, fn.ID,
	)
	return err
}

func (d *DB) DeleteFunction(id string) error {
	_, err := d.db.Exec(`DELETE FROM functions WHERE id = ?`, id)
	return err
}

func (d *DB) CreateInvocation(inv *protocol.Invocation) error {
	_, err := d.db.Exec(
		`INSERT INTO invocations (id, function_id, status, duration_ms, started_at, error_message)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.FunctionID, inv.Status, inv.DurationMS, inv.StartedAt, inv.ErrorMessage,
	)
	return err
}

func (d *DB) GetInvocations(functionID string, limit int) ([]*protocol.Invocation, error) {
	rows, err := d.db.Query(
		`SELECT id, function_id, status, duration_ms, started_at, error_message
		 FROM invocations WHERE function_id = ? ORDER BY started_at DESC LIMIT ?`,
		functionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	invocations := make([]*protocol.Invocation, 0)
	for rows.Next() {
		var inv protocol.Invocation
		var errorMsg sql.NullString
		err := rows.Scan(&inv.ID, &inv.FunctionID, &inv.Status, &inv.DurationMS, &inv.StartedAt, &errorMsg)
		if err != nil {
			return nil, err
		}
		if errorMsg.Valid {
			inv.ErrorMessage = errorMsg.String
		}
		invocations = append(invocations, &inv)
	}
	return invocations, nil
}
