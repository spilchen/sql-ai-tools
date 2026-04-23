// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build windows

package versionroute

import (
	"errors"
	"os"
	"os/exec"
)

// execBackend runs the backend at path as a child process and exits
// the current process with the child's exit code. Windows lacks an
// equivalent of POSIX execve, so we cannot truly replace this process
// — the cost is one extra process in the chain, which is invisible to
// most callers.
//
// stdio and environment are inherited. Parent-side signal forwarding
// is not implemented (Go's os/exec does not propagate signals
// received by the parent to the child). On Windows interactive
// console use, Ctrl-C/Ctrl-Break reaches the child via the shared
// console group, so terminal users see the expected behavior; signals
// dispatched programmatically to the parent are not relayed.
//
// The args[1:] slice is forwarded (Cmd injects its own argv[0] from
// path), so the child's os.Args[0] is the backend's real path rather
// than the parent's argv[0]. The child's quarterFromExecutable
// consults os.Executable() so this distinction does not affect
// routing.
func execBackend(path string, args []string) error {
	cmd := exec.Command(path, args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	err := cmd.Run()
	if err == nil {
		os.Exit(0)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return err
}
