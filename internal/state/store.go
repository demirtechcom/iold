package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("deployment not found")
	ErrConflict     = errors.New("deployment changed concurrently")
	ErrDuplicate    = errors.New("deployment already exists")
	ErrNotDestroyed = errors.New("deployment is not DESTROYED")
)

type Deployment struct {
	ID               string
	Alias            string
	Artifact         string
	ArtifactRevision string
	ModelCacheDir    string
	Port             int
	PID              int
	Command          string // command line recorded at process start, for PID ownership checks
	StartedAt        time.Time
	StartToken       string // OS-native process start identity (for example Linux /proc start ticks)
	Phase            Phase
	FailureReason    string
	IdempotencyKey   string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Migrations only ever append; PRAGMA user_version records how many have
// been applied so existing databases upgrade in order.
var migrations = []string{
	`CREATE TABLE deployments (
		id                TEXT PRIMARY KEY,
		alias             TEXT NOT NULL,
		artifact          TEXT NOT NULL,
		artifact_revision TEXT NOT NULL,
		port              INTEGER NOT NULL,
		pid               INTEGER NOT NULL DEFAULT 0,
		phase             TEXT NOT NULL,
		failure_reason    TEXT NOT NULL DEFAULT '',
		idempotency_key   TEXT NOT NULL,
		created_at        TEXT NOT NULL,
		updated_at        TEXT NOT NULL
	)`,
	`ALTER TABLE deployments ADD COLUMN command TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE deployments ADD COLUMN started_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE deployments ADD COLUMN start_token TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE deployments ADD COLUMN model_cache_dir TEXT NOT NULL DEFAULT ''`,
}

type Store struct {
	db *sql.DB
}

// Open creates the state directory and database with restrictive
// permissions and applies any pending migrations transactionally.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secure state dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create state file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure state file: %w", err)
	}
	file.Close()

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// The modernc driver is not safe for concurrent writes on one
	// connection pool with multiple conns; a single conn also keeps
	// transactions strictly serialized for a single-user CLI.
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("state db schema version %d is newer than this binary supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump schema version to %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Create inserts a new deployment in phase REQUESTED.
func (s *Store) Create(d Deployment) (Deployment, error) {
	now := time.Now().UTC().Truncate(time.Second)
	d.Phase = PhaseRequested
	d.FailureReason = ""
	d.CreatedAt = now
	d.UpdatedAt = now
	_, err := s.db.Exec(`INSERT INTO deployments
		(id, alias, artifact, artifact_revision, model_cache_dir, port, pid, command, started_at, start_token, phase, failure_reason, idempotency_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Alias, d.Artifact, d.ArtifactRevision, d.ModelCacheDir, d.Port, d.PID, d.Command,
		formatOptionalTime(d.StartedAt), d.StartToken, string(d.Phase),
		d.FailureReason, d.IdempotencyKey, d.CreatedAt.Format(time.RFC3339), d.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		if isUniqueViolation(err) {
			return Deployment{}, fmt.Errorf("%w: %s", ErrDuplicate, d.ID)
		}
		return Deployment{}, err
	}
	return d, nil
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite does not export a stable error type for
	// constraint violations; match the SQLite error text.
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

func (s *Store) Get(id string) (Deployment, error) {
	row := s.db.QueryRow(`SELECT id, alias, artifact, artifact_revision, model_cache_dir, port, pid, command, started_at, start_token, phase,
		failure_reason, idempotency_key, created_at, updated_at
		FROM deployments WHERE id = ?`, id)
	return scanDeployment(row)
}

func (s *Store) List() ([]Deployment, error) {
	rows, err := s.db.Query(`SELECT id, alias, artifact, artifact_revision, model_cache_dir, port, pid, command, started_at, start_token, phase,
		failure_reason, idempotency_key, created_at, updated_at
		FROM deployments ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDeployment(row scanner) (Deployment, error) {
	var d Deployment
	var phase, startedAt, createdAt, updatedAt string
	err := row.Scan(&d.ID, &d.Alias, &d.Artifact, &d.ArtifactRevision, &d.ModelCacheDir, &d.Port, &d.PID,
		&d.Command, &startedAt, &d.StartToken, &phase, &d.FailureReason, &d.IdempotencyKey,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.Phase = Phase(phase)
	if d.StartedAt, err = parseOptionalTime(startedAt); err != nil {
		return Deployment{}, fmt.Errorf("corrupt started_at %q: %w", startedAt, err)
	}
	if d.CreatedAt, err = time.Parse(time.RFC3339, createdAt); err != nil {
		return Deployment{}, fmt.Errorf("corrupt created_at %q: %w", createdAt, err)
	}
	if d.UpdatedAt, err = time.Parse(time.RFC3339, updatedAt); err != nil {
		return Deployment{}, fmt.Errorf("corrupt updated_at %q: %w", updatedAt, err)
	}
	return d, nil
}

// Transition moves a deployment from `from` to `to` atomically. It fails
// with ErrIllegalTransition for edges the state machine forbids, and with
// ErrConflict if the stored phase is no longer `from` (compare-and-set).
// reason is stored only for terminal failure/reconciliation states.
func (s *Store) Transition(id string, from, to Phase, reason string) error {
	if err := checkTransition(from, to); err != nil {
		return err
	}
	if to != PhaseFailed && to != PhaseCrashed {
		reason = ""
	}
	res, err := s.db.Exec(`UPDATE deployments
		SET phase = ?, failure_reason = ?, updated_at = ?
		WHERE id = ? AND phase = ?`,
		string(to), reason, time.Now().UTC().Format(time.RFC3339), id, string(from))
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		current, err := s.Get(id)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: expected phase %s, found %s", ErrConflict, from, current.Phase)
	}
	return nil
}

// SetRuntimeIntent persists the exact command and selected port before the
// process side effect. Recovery can use this STARTING intent to discover a
// child if the CLI dies between exec and SetRuntime.
func (s *Store) SetRuntimeIntent(id string, port int, command string) error {
	res, err := s.db.Exec(`UPDATE deployments
		SET pid = 0, port = ?, command = ?, started_at = '', start_token = '', updated_at = ?
		WHERE id = ? AND phase = ?`,
		port, command, time.Now().UTC().Format(time.RFC3339Nano), id, string(PhaseDownloading))
	if err != nil {
		return err
	}
	return expectOneRuntimeRow(s, res, id, PhaseDownloading)
}

// SetRuntime records the verified OS identity of a supervised process. The
// phase/PID guard prevents a concurrent lifecycle operation from attaching a
// process to a deployment that has already moved on.
func (s *Store) SetRuntime(id string, pid, port int, command string, startedAt time.Time, startToken string) error {
	res, err := s.db.Exec(`UPDATE deployments
		SET pid = ?, port = ?, command = ?, started_at = ?, start_token = ?, updated_at = ?
		WHERE id = ? AND phase = ? AND pid = 0`,
		pid, port, command, formatOptionalTime(startedAt), startToken,
		time.Now().UTC().Format(time.RFC3339Nano), id, string(PhaseStarting))
	if err != nil {
		return err
	}
	return expectOneRuntimeRow(s, res, id, PhaseStarting)
}

func expectOneRuntimeRow(s *Store, res sql.Result, id string, expected Phase) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		current, getErr := s.Get(id)
		if errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		if getErr != nil {
			return getErr
		}
		return fmt.Errorf("%w: expected phase %s with no runtime, found phase %s pid %d",
			ErrConflict, expected, current.Phase, current.PID)
	}
	return nil
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

// Delete removes a deployment record. Only DESTROYED deployments may be
// deleted so ownership history is never dropped while resources can exist.
func (s *Store) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM deployments WHERE id = ? AND phase = ?`,
		id, string(PhaseDestroyed))
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		if _, err := s.Get(id); errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("%w: %s", ErrNotDestroyed, id)
	}
	return nil
}
