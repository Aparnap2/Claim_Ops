package liteparse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const (
	// MaxStdoutBytes caps shim stdout at 64MB. Anything larger is a
	// runaway payload, not a document.
	MaxStdoutBytes = 64 << 20
	// maxStderrBytes keeps failure diagnostics bounded.
	maxStderrBytes = 1 << 20
	// DefaultPythonBin is the interpreter used to run the shim.
	DefaultPythonBin = "python3"
)

// Runner executes the shim against a trusted PDF file and returns its
// raw stdout JSON. Implementations must honor ctx cancellation (killing
// the subprocess) and bound stdout to MaxStdoutBytes.
type Runner interface {
	Run(ctx context.Context, pdfPath string) (rawJSON []byte, err error)
}

// ExecRunner is the production Runner: it spawns
// `python3 shim.py <pdfPath>` via exec.CommandContext so ctx
// cancellation kills the subprocess.
type ExecRunner struct {
	pythonBin string
	shimPath  string
	timeout   time.Duration
}

// NewExecRunner builds an ExecRunner. Empty pythonBin selects
// DefaultPythonBin, empty shimPath selects DefaultShimPath, and
// timeout <= 0 selects DefaultTimeout (applied only when ctx carries no
// deadline of its own).
func NewExecRunner(pythonBin, shimPath string, timeout time.Duration) *ExecRunner {
	if pythonBin == "" {
		pythonBin = DefaultPythonBin
	}
	if shimPath == "" {
		shimPath = DefaultShimPath
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &ExecRunner{pythonBin: pythonBin, shimPath: shimPath, timeout: timeout}
}

// Run implements Runner.
func (r *ExecRunner) Run(ctx context.Context, pdfPath string) ([]byte, error) {
	runCtx := ctx
	cancel := context.CancelFunc(func() {})
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		runCtx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, r.pythonBin, r.shimPath, pdfPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, wrapExec("stdout pipe", err, nil, ctx)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, wrapExec("start", err, nil, ctx)
	}
	// Bound stdout: read at most MaxStdoutBytes+1 to detect overflow.
	limited := io.LimitReader(stdout, MaxStdoutBytes+1)
	raw, readErr := io.ReadAll(limited)
	waitErr := cmd.Wait()

	if readErr != nil {
		return nil, wrapExec("read stdout", readErr, stderr.Bytes(), ctx)
	}
	if int64(len(raw)) > MaxStdoutBytes {
		return nil, fmt.Errorf("%w: shim stdout exceeds %d bytes", errShimOverflow, MaxStdoutBytes)
	}
	if waitErr != nil {
		return nil, mapExitError(runCtx, waitErr, stderr.Bytes())
	}
	return raw, nil
}
