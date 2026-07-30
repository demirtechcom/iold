package state

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "iold.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func sample() Deployment {
	return Deployment{
		ID:               "dep-1",
		Alias:            "qwen3.6-35b-a3b",
		Artifact:         "unsloth/Qwen3.6-35B-A3B-NVFP4-Fast",
		ArtifactRevision: "MOCK_REVISION_PIN_PENDING_GPU_VALIDATION",
		Port:             8000,
		IdempotencyKey:   "idem-1",
	}
}

func TestCreateGetRoundtrip(t *testing.T) {
	store, _ := openTestStore(t)
	created, err := store.Create(sample())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Phase != PhaseRequested {
		t.Fatalf("new deployment phase = %s, want REQUESTED", created.Phase)
	}
	got, err := store.Get("dep-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != created {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, created)
	}
}

func TestCreateDuplicateFails(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := store.Create(sample()); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestHappyPathTransitions(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	path := []Phase{
		PhaseValidating, PhaseDownloading, PhaseStarting,
		PhaseHealthy, PhaseRegistering, PhaseReady,
		PhaseDestroying, PhaseDestroyed,
	}
	from := PhaseRequested
	for _, to := range path {
		if err := store.Transition("dep-1", from, to, ""); err != nil {
			t.Fatalf("Transition %s -> %s: %v", from, to, err)
		}
		from = to
	}
	got, err := store.Get("dep-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Phase != PhaseDestroyed {
		t.Fatalf("final phase = %s, want DESTROYED", got.Phase)
	}
}

func TestIllegalTransitionRejectedWithoutWrite(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := store.Transition("dep-1", PhaseRequested, PhaseHealthy, "")
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
	got, _ := store.Get("dep-1")
	if got.Phase != PhaseRequested {
		t.Fatalf("phase changed to %s after rejected transition", got.Phase)
	}
}

func TestTransitionCompareAndSetConflict(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Transition("dep-1", PhaseRequested, PhaseValidating, ""); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	err := store.Transition("dep-1", PhaseRequested, PhaseValidating, "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestAnyPhaseCanFailWithReasonExceptTerminal(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Transition("dep-1", PhaseRequested, PhaseFailed, "vllm exited during startup"); err != nil {
		t.Fatalf("Transition to FAILED: %v", err)
	}
	got, _ := store.Get("dep-1")
	if got.FailureReason != "vllm exited during startup" {
		t.Fatalf("failure reason = %q", got.FailureReason)
	}
	if err := store.Transition("dep-1", PhaseFailed, PhaseDestroying, ""); err != nil {
		t.Fatalf("FAILED -> DESTROYING: %v", err)
	}
	if err := store.Transition("dep-1", PhaseDestroying, PhaseDestroyed, ""); err != nil {
		t.Fatalf("DESTROYING -> DESTROYED: %v", err)
	}
	if err := store.Transition("dep-1", PhaseDestroyed, PhaseFailed, "x"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("DESTROYED -> FAILED should be illegal, got %v", err)
	}
}

func TestDeleteRequiresDestroyed(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete("dep-1"); !errors.Is(err, ErrNotDestroyed) {
		t.Fatalf("expected ErrNotDestroyed, got %v", err)
	}
	if err := store.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	mustDestroy(t, store, "dep-1", PhaseRequested)
	if err := store.Delete("dep-1"); err != nil {
		t.Fatalf("Delete after DESTROYED: %v", err)
	}
	if _, err := store.Get("dep-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deployment still present after delete: %v", err)
	}
}

func mustDestroy(t *testing.T, store *Store, id string, from Phase) {
	t.Helper()
	for _, step := range []struct{ from, to Phase }{
		{from, PhaseFailed},
		{PhaseFailed, PhaseDestroying},
		{PhaseDestroying, PhaseDestroyed},
	} {
		if err := store.Transition(id, step.from, step.to, "test teardown"); err != nil {
			t.Fatalf("Transition %s -> %s: %v", step.from, step.to, err)
		}
	}
}

func TestStatePersistsAcrossReopen(t *testing.T) {
	store, path := openTestStore(t)
	if _, err := store.Create(sample()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, step := range []struct{ from, to Phase }{
		{PhaseRequested, PhaseValidating},
		{PhaseValidating, PhaseDownloading},
	} {
		if err := store.Transition("dep-1", step.from, step.to, ""); err != nil {
			t.Fatalf("Transition: %v", err)
		}
	}
	if err := store.SetRuntimeIntent("dep-1", 8001, "vllm serve x --port 8001"); err != nil {
		t.Fatalf("SetRuntimeIntent: %v", err)
	}
	if err := store.Transition("dep-1", PhaseDownloading, PhaseStarting, ""); err != nil {
		t.Fatalf("Transition to STARTING: %v", err)
	}
	startedAt := time.Date(2026, 7, 30, 10, 20, 30, 123, time.UTC)
	if err := store.SetRuntime("dep-1", 4242, 8001, "vllm serve x --port 8001", startedAt, "linux:12345"); err != nil {
		t.Fatalf("SetRuntime: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.Get("dep-1")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Phase != PhaseStarting || got.PID != 4242 || got.Port != 8001 ||
		!got.StartedAt.Equal(startedAt) || got.StartToken != "linux:12345" {
		t.Fatalf("state lost across reopen: %+v", got)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	_, path := openTestStore(t)
	for range 2 {
		store, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		store.Close()
	}
}

func TestMigrationAddsProcessIdentityToLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iold.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO deployments
		(id, alias, artifact, artifact_revision, port, pid, command, phase, failure_reason, idempotency_key, created_at, updated_at)
		VALUES ('legacy', 'legacy', 'org/model', 'rev', 8000, 0, '', 'REQUESTED', '', 'idem', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer store.Close()
	deployment, err := store.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if !deployment.StartedAt.IsZero() || deployment.StartToken != "" {
		t.Fatalf("legacy identity should migrate empty: %+v", deployment)
	}
}

func TestListOrdersByCreation(t *testing.T) {
	store, _ := openTestStore(t)
	first := sample()
	second := sample()
	second.ID = "dep-2"
	second.IdempotencyKey = "idem-2"
	for _, d := range []Deployment{first, second} {
		if _, err := store.Create(d); err != nil {
			t.Fatalf("Create %s: %v", d.ID, err)
		}
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != "dep-1" || list[1].ID != "dep-2" {
		t.Fatalf("unexpected list: %+v", list)
	}
}
