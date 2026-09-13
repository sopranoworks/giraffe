// Package limits enforces the resource ceilings the whole giraffe process
// runs under: virtual memory, open file descriptors, and wall-clock time.
// See specifications/giraffe.md §3: a prior LSP incident exhausted host
// memory and, separately, file descriptors. This is what makes the failure
// mode "the command dies", never "the host dies".
package limits

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

// Config is the set of ceilings applied to the process.
type Config struct {
	VirtualMemory uint64        // bytes, RLIMIT_AS — outer backstop (§3 C3, §20 C-4)
	HeapLimit     uint64        // bytes, GOMEMLIMIT — the meaningful heap ceiling (§20 C-4)
	OpenFiles     uint64        // count, RLIMIT_NOFILE
	Timeout       time.Duration // wall-clock budget for the invocation
}

// Default returns the ceilings used when no override is set.
//
// The specification fixes the mechanism (ulimit -v, ulimit -n, a timeout)
// and says open files should be "tens, not thousands", but gives no number
// for memory or the timeout.
//
// Measured empirically (Go 1.26, linux/amd64), RLIMIT_AS turns out to
// track the Go runtime's own virtual-address-space bookkeeping, not actual
// memory use: a bare `func main(){}` needs ~704 MiB just to start, and a
// full `find` run against gekka (380 files, ~100k lines) needed a ceiling
// between 1152 and 1280 MiB to complete — while its actual peak resident
// set size (measured with /usr/bin/time -v) was 16 MiB. The gap is ~80x
// and is Go's runtime reserving virtual address space for goroutine
// stacks and heap arenas (scaling with concurrency, not with real heap
// usage), not evidence of real memory pressure. See the Step 6 report for
// the full measurement and what this implies for RLIMIT_AS as a fit for a
// Go binary.
//
// 4 GiB leaves headroom over the measured 1.2-1.3 GiB floor for a
// larger repository than gekka, while staying an order of magnitude below
// the 64 GB that caused the incident §3 describes. This is still a
// starting value: it has one real data point behind it, not a range of
// repository sizes.
// defaultHeapLimit (GOMEMLIMIT, §20 C-4) is a soft ceiling, unlike
// RLIMIT_AS: the runtime GCs harder to try to stay under it, but does not
// kill the process for exceeding it. This matters for what "verify by
// exceeding it deliberately" (directive 002, mirroring 001's Step 1 rule)
// can actually mean here — see Enforce's comment and the report.
//
// Measured against gekka (Go 1.26, linux/amd64), cold, `GODEBUG=gctrace=1`
// and repeated trials at each ceiling:
//
//	unrestricted / 512-32 MiB : ~0.06s (baseline, no measurable GC pressure)
//	16 MiB                    : ~0.11s (~2x)
//	8-1 MiB                   : ~0.28-0.32s (~5x, then flat — GC cannot
//	                            squeeze further no matter how low the
//	                            ceiling goes)
//
// Every one of those trials completed and produced correct results —
// GOMEMLIMIT alone never failed the command, down to 1 MiB, confirming
// it is a scheduling pressure, not a hard cap: RLIMIT_AS above is what
// actually protects the host if a heap runs away.
//
// 64 MiB sat at 2x the last ceiling that showed no slowdown against
// gekka (32 MiB) — real headroom without the ~250x-too-loose problem
// RLIMIT_AS has for this program (see its own doc comment above), but
// calibrated against exactly one corpus's footprint.
//
// §29 C-43: it stopped being enough the moment a second, larger corpus
// was measured against it. Directive 010 added methods and fields,
// growing kubernetes' live-entry heap from ~50 MB to ~76 MB — both
// already at or past 64 MiB, but 76 MB has no room left for the
// runtime to work with at all. GOMEMLIMIT does not fail in that
// state; it GCs continuously trying to reach a goal already below the
// heap's own live floor (`GODEBUG=gctrace=1` showed 383 collections
// in 1.5s against 49 before, each spinning up GOMAXPROCS worker
// goroutines — expensive in its own right, and severely so under host
// contention). Directive 011's own no-match measurement had first
// attributed kubernetes' 0.37s→1.4s regression to query.Suggest,
// following directive 010's diagnosis; every isolated component
// (index.Load, index.Refresh, query.Suggest, query.Search) in fact
// scaled proportionately to kubernetes' 1.92x entry growth, and this
// was the actual, disproportionate cause underneath all of them.
//
// Re-measured the same way §20 C-4 originally was — cold,
// `GODEBUG=gctrace=1`, repeated trials — but against kubernetes,
// the corpus that actually exercises this ceiling:
//
//	64-192 MiB : 1.51s → 0.65s → 0.43s (falling as the ceiling nears
//	             the live floor)
//	256 MiB    : 0.42s (first ceiling with no measurable slowdown —
//	             matches the pre-item-2, no-methods-and-fields baseline)
//	384-768 MiB: 0.41-0.44s (flat)
//
// 512 MiB sits at 2x 256 MiB, the same "2x the last ceiling that
// showed no slowdown" rule the original 64 MiB used, applied to the
// corpus that now needs it. gekka and llvm-project's own live floors
// (§27, §29: well under 64 MiB and ~48-53 MB respectively) sit far
// inside this ceiling too, so neither sees any behaviour change.
const defaultHeapLimit = 512 << 20 // 512 MiB

// HeapLimit (GOMEMLIMIT) is chosen separately — see its own doc comment
// below the measurement that produced it.
func Default() Config {
	return Config{
		VirtualMemory: 4 << 30, // 4 GiB
		HeapLimit:     defaultHeapLimit,
		OpenFiles:     64,
		Timeout:       30 * time.Second,
	}
}

// FromEnv applies Default, then overrides any field with a matching
// GIRAFFE_LIMIT_* environment variable. It exists so the ceilings can be
// exercised deliberately — see the directive's Step 1 verification — and
// tuned once Step 6 produces evidence, without a rebuild.
func FromEnv() (Config, error) {
	cfg := Default()
	if v := os.Getenv("GIRAFFE_LIMIT_MEM_BYTES"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("GIRAFFE_LIMIT_MEM_BYTES: %w", err)
		}
		cfg.VirtualMemory = n
	}
	if v := os.Getenv("GIRAFFE_LIMIT_HEAP_BYTES"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("GIRAFFE_LIMIT_HEAP_BYTES: %w", err)
		}
		cfg.HeapLimit = n
	}
	if v := os.Getenv("GIRAFFE_LIMIT_OPEN_FILES"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("GIRAFFE_LIMIT_OPEN_FILES: %w", err)
		}
		cfg.OpenFiles = n
	}
	if v := os.Getenv("GIRAFFE_LIMIT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("GIRAFFE_LIMIT_TIMEOUT: %w", err)
		}
		cfg.Timeout = d
	}
	return cfg, nil
}

// Enforce sets the process's own virtual memory and open-file ceilings —
// the effect a shell's `ulimit` has on a command it then execs, applied
// here directly since this process never re-execs itself — and returns a
// context that expires after cfg.Timeout.
//
// It must be called before any real work starts. A zero Config sets zero
// ceilings and fails immediately on first use: the intended fallback for a
// misconfigured wrapper is failing loudly, not running unbounded.
func Enforce(ctx context.Context, cfg Config) (context.Context, context.CancelFunc, error) {
	mem := syscall.Rlimit{Cur: cfg.VirtualMemory, Max: cfg.VirtualMemory}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &mem); err != nil {
		return nil, nil, fmt.Errorf("set memory limit to %d bytes: %w", cfg.VirtualMemory, err)
	}
	// GOMEMLIMIT (§20 C-4): a soft heap ceiling, unlike RLIMIT_AS. The
	// runtime GCs more aggressively to try to stay under it, but does not
	// kill the process for exceeding it — see the doc comment on
	// defaultHeapLimit for what this means for "exceeding it fails the
	// command" here. RLIMIT_AS above remains the hard backstop.
	debug.SetMemoryLimit(int64(cfg.HeapLimit))
	files := syscall.Rlimit{Cur: cfg.OpenFiles, Max: cfg.OpenFiles}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &files); err != nil {
		return nil, nil, fmt.Errorf("set open-file limit to %d: %w", cfg.OpenFiles, err)
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	return ctx, cancel, nil
}

// Watch force-exits the process with a timeout message if ctx expires
// before done is closed. Call it in a goroutine right after Enforce; close
// done when the real work finishes normally.
//
// A cooperative deadline — checking ctx.Err() in loops — only bounds code
// that checks. Watch is the backstop for code that does not, or cannot (a
// single long call into a library). os.Exit ends the process immediately
// regardless of what any other goroutine is doing.
//
// §21 C-7: GOMEMLIMIT is soft — a runaway heap under a low ceiling does
// not fail on its own, it GCs harder, which stalls the command until this
// timeout fires. Without a hint, that surfaces as a bare "timed out" with
// no way to tell a slow query from a heap fight. The message names
// GOMEMLIMIT (cfg.HeapLimit) as a suspect and reports GCCPUFraction — the
// fraction of CPU time spent in GC, cheap to read from runtime.MemStats
// and a direct measure of the pressure that composition depends on.
func Watch(ctx context.Context, done <-chan struct{}, cfg Config) {
	select {
	case <-done:
	case <-ctx.Done():
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fmt.Fprintf(os.Stderr,
			"giraffe: timed out (suspect: GOMEMLIMIT=%d bytes — a low heap ceiling causes GC thrashing that surfaces as a timeout, not an allocation failure; GC CPU fraction at timeout: %.1f%%, %d collections so far)\n",
			cfg.HeapLimit, m.GCCPUFraction*100, m.NumGC)
		os.Exit(1)
	}
}
