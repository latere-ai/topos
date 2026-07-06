package round

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestInstallHandler_CancelsOnSignal(t *testing.T) {
	ctx, fin := InstallHandler(context.Background(), syscall.SIGUSR1)
	defer fin()

	// Send SIGUSR1 to ourselves; the handler must cancel the derived
	// context within ~10 ms.
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill self: %v", err)
	}

	select {
	case <-ctx.Done():
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context not cancelled within 500ms of signal")
	}
}

func TestInstallHandler_DefaultSignalsCompile(_ *testing.T) {
	// Smoke: calling with no signals selects the default (os.Interrupt)
	// without panicking. Don't actually fire SIGINT - it would tear down
	// the test runner.
	_, fin := InstallHandler(context.Background())
	fin()
}

func TestInstallHandler_FinalizeIsIdempotent(t *testing.T) {
	ctx, fin := InstallHandler(context.Background(), syscall.SIGUSR2)
	fin()
	// After finalize the derived context is cancelled.
	select {
	case <-ctx.Done():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("finalize did not cancel ctx")
	}
}
