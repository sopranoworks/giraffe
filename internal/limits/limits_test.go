package limits

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Setrlimit only ever lowers the calling process's own ceiling and cannot
// be raised back without privilege, so exercising it in the test process
// itself would cripple every later test in this package. Each case below
// instead re-execs this test binary as a subprocess that deliberately
// exceeds one ceiling, and the parent asserts the subprocess failed.

const reexecEnv = "GIRAFFE_LIMITS_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(reexecEnv) {
	case "memory":
		helperExceedMemory()
	case "openfiles":
		helperExceedOpenFiles()
	case "timeout":
		helperExceedTimeout()
	case "heap":
		helperExceedHeapLimit()
	default:
		os.Exit(m.Run())
	}
}

func helperExceedMemory() {
	_, cancel, err := Enforce(context.Background(), Config{VirtualMemory: 16 << 20, OpenFiles: 64, Timeout: 5 * time.Second})
	if err != nil {
		os.Exit(2)
	}
	defer cancel()
	var chunks [][]byte
	for i := 0; i < 1000; i++ {
		chunks = append(chunks, make([]byte, 8<<20)) // 8MiB steps, well past a 16MiB ceiling
		chunks[len(chunks)-1][0] = 1                 // touch it so it cannot be optimised away
	}
	os.Exit(0) // must not be reached
}

func helperExceedOpenFiles() {
	_, cancel, err := Enforce(context.Background(), Config{VirtualMemory: Default().VirtualMemory, OpenFiles: 8, Timeout: 5 * time.Second})
	if err != nil {
		os.Exit(2)
	}
	defer cancel()
	for i := 0; i < 64; i++ {
		if _, err := os.Open("limits.go"); err != nil {
			os.Exit(1) // expected: the open-file ceiling was reached
		}
	}
	os.Exit(0) // must not be reached
}

func helperExceedTimeout() {
	cfg := Config{VirtualMemory: Default().VirtualMemory, HeapLimit: 77 << 20, OpenFiles: 64, Timeout: 200 * time.Millisecond}
	ctx, cancel, err := Enforce(context.Background(), cfg)
	if err != nil {
		os.Exit(2)
	}
	defer cancel()
	done := make(chan struct{})
	go Watch(ctx, done, cfg, nil) // never closed: simulates work that outlives the timeout
	time.Sleep(5 * time.Second)
}

func helperExceedHeapLimit() {
	// GOMEMLIMIT is a soft ceiling (§20 C-4): the runtime GCs harder to
	// try to stay under it but does not kill the process for exceeding
	// it. This helper deliberately allocates well past a 1 MiB ceiling
	// and is expected to survive — unlike helperExceedMemory's RLIMIT_AS,
	// which is a hard one.
	cfg := Config{VirtualMemory: Default().VirtualMemory, HeapLimit: 1 << 20, OpenFiles: 64, Timeout: 5 * time.Second}
	_, cancel, err := Enforce(context.Background(), cfg)
	if err != nil {
		os.Exit(2)
	}
	defer cancel()
	var chunks [][]byte
	for i := 0; i < 4; i++ {
		chunks = append(chunks, make([]byte, 4<<20)) // 16 MiB total, 16x the 1 MiB ceiling
		chunks[len(chunks)-1][0] = 1
	}
	fmt.Println("survived")
	os.Exit(0) // the expected outcome, unlike helperExceedMemory
}

func runHelper(t *testing.T, mode string) (exitErr error, output string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{os.Args[0]}
	cmd.Env = append(os.Environ(), reexecEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	return err, string(out)
}

func TestEnforce_MemoryLimitFailsTheCommand(t *testing.T) {
	err, out := runHelper(t, "memory")
	if err == nil {
		t.Fatalf("expected the command to fail once it exceeded the memory ceiling; it exited 0, output: %s", out)
	}
	if !strings.Contains(out, "out of memory") && !strings.Contains(out, "cannot allocate memory") {
		t.Logf("command failed as expected (exit: %v), output: %s", err, out)
	}
}

func TestEnforce_OpenFilesLimitFailsTheCommand(t *testing.T) {
	err, out := runHelper(t, "openfiles")
	if err == nil {
		t.Fatalf("expected the command to fail once it exceeded the open-files ceiling; it exited 0, output: %s", out)
	}
	t.Logf("command failed as expected (exit: %v), output: %s", err, out)
}

func TestEnforce_TimeoutFailsTheCommand(t *testing.T) {
	start := time.Now()
	err, out := runHelper(t, "timeout")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected the command to fail once its timeout elapsed; it exited 0, output: %s", out)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("command ran for %s, close to the 5s sleep: the timeout did not cut it short", elapsed)
	}
	t.Logf("command was killed by its timeout after %s (exit: %v), output: %s", elapsed, err, out)
}

// TestEnforce_TimeoutNamesGOMEMLIMITAsASuspect (§21 C-7): a bare "timed
// out" gives no way to tell a slow query from GOMEMLIMIT-driven GC
// thrashing. The message must name the configured GOMEMLIMIT value and a
// GC-pressure indicator, so the failure is diagnosable.
func TestEnforce_TimeoutNamesGOMEMLIMITAsASuspect(t *testing.T) {
	_, out := runHelper(t, "timeout")
	if !strings.Contains(out, "GOMEMLIMIT") {
		t.Fatalf("expected the timeout message to name GOMEMLIMIT as a suspect, got: %s", out)
	}
	// 77 << 20 = 80,740,352 bytes; assert the configured value itself
	// appears, not just the word GOMEMLIMIT.
	if !strings.Contains(out, "80740352") {
		t.Fatalf("expected the configured GOMEMLIMIT value (80740352 bytes) in the message, got: %s", out)
	}
	if !strings.Contains(out, "GC CPU fraction") {
		t.Fatalf("expected a GC-pressure indicator alongside the suspicion, got: %s", out)
	}
}

// TestEnforce_HeapLimitIsSoft_ExceedingItDoesNotFailTheCommand documents
// a real, measured difference from the other three ceilings (§20 C-4): a
// process that allocates 16x past GOMEMLIMIT survives, GC'ing harder
// instead of dying. RLIMIT_AS (TestEnforce_MemoryLimitFailsTheCommand)
// remains the ceiling that actually protects the host; GOMEMLIMIT only
// reduces peak heap short of that.
func TestEnforce_HeapLimitIsSoft_ExceedingItDoesNotFailTheCommand(t *testing.T) {
	err, out := runHelper(t, "heap")
	if err != nil {
		t.Fatalf("expected the command to survive exceeding GOMEMLIMIT (it is a soft limit): %v, output: %s", err, out)
	}
	if !strings.Contains(out, "survived") {
		t.Fatalf("expected the subprocess to complete normally, got: %s", out)
	}
}
