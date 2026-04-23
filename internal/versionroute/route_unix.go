// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

//go:build !windows

package versionroute

import (
	"os"
	"syscall"
)

// execBackend replaces the current process with the backend at path.
// On success this call does not return; on failure (e.g. EACCES on
// the chosen file, ENOENT despite the prior FindBackend hit due to a
// race) it returns the syscall error so the caller can surface it.
//
// args is forwarded verbatim, so the child sees the same os.Args[0]
// the user typed (often a full path like /usr/local/bin/crdb-sql,
// not the bare string "crdb-sql"). This is fine because the child's
// quarterFromExecutable consults os.Executable() — the real on-disk
// sibling path — not argv[0]. A renamed-on-disk parent therefore
// cannot induce a wrong-quarter exec loop.
func execBackend(path string, args []string) error {
	return syscall.Exec(path, args, os.Environ())
}
