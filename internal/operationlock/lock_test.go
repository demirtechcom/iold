package operationlock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestTryAcquireReportsBusyUntilRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("TryAcquire err = %v, want ErrBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after release: %v", err)
	}
	second.Close()
}
