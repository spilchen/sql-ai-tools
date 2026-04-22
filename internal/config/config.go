// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package config loads the optional crdb-sql.yaml project file that
// maps schema-file globs to query-file globs. When a config is present,
// schema-aware subcommands (currently validate) can run with no flags
// and operate over every matching query file in the project.
//
// The file lives at the project root and is auto-discovered from the
// current working directory by the root command's PersistentPreRunE.
// Absence of a config file is not an error — Discover returns
// (nil, nil) and the subcommand falls back to its existing flag-driven
// behavior.
//
// The schema is intentionally small (one version, repeating
// schema/queries pairs); richer fields (per-pair name, default tier,
// connection profile) can be added in future versions without breaking
// readers that already understand version 1.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// SupportedVersion is the only schema version this build understands.
// Bumping it is a breaking change for older binaries; introduce a new
// version only when a field's meaning changes incompatibly.
const SupportedVersion = 1

// DefaultFilenames are the names Discover looks for in the working
// directory, in priority order. Both spellings are accepted because
// .yaml and .yml are equally common in Go projects.
var DefaultFilenames = []string{"crdb-sql.yaml", "crdb-sql.yml"}

// MaxConfigFileSize caps the size of a config file Load will read.
// Configs are tiny by nature; the limit exists only to prevent a
// pathological 1GB YAML file from being slurped into memory.
const MaxConfigFileSize = 1 << 20 // 1 MiB

// File is the parsed crdb-sql.yaml. It is populated by Load (and
// Discover) and then attached to the root command's per-invocation
// state so subcommands can read it.
//
// BaseDir is set by the loader to the directory containing the YAML
// file. Globs in SQL[*].Schema and SQL[*].Queries are resolved
// relative to BaseDir, which makes configs portable across machines
// (no absolute paths required) and makes tests easy to write
// (point the loader at t.TempDir()).
type File struct {
	Version int       `yaml:"version"`
	SQL     []SQLPair `yaml:"sql"`

	// BaseDir is the directory the config file was loaded from. Not
	// present in the YAML; populated by Load. Empty for File values
	// constructed in tests without going through Load — those tests
	// must use absolute glob patterns.
	BaseDir string `yaml:"-"`
}

// SQLPair maps a set of CREATE TABLE schema files to the queries that
// should be validated against that schema. Both sides are lists of
// glob patterns (doublestar syntax: `**` matches any depth).
//
// Multiple pairs in one config let a project keep, e.g., production
// schema with its queries separate from a test fixture schema with
// its own queries.
type SQLPair struct {
	Schema  []string `yaml:"schema"`
	Queries []string `yaml:"queries"`
}

// Load reads and parses the config file at path. The returned *File
// has BaseDir set to filepath.Dir(path) so subsequent glob expansion
// resolves patterns relative to the config's location.
//
// Load is strict: unknown YAML fields produce an error rather than
// being silently dropped. This catches typos in the config (e.g.
// `querys:` instead of `queries:`) at load time instead of producing
// a confusing empty match later.
func Load(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config file %s: %w", path, err)
	}
	if info.Size() > MaxConfigFileSize {
		return nil, fmt.Errorf("config file %s is too large (%d bytes, max %d)",
			path, info.Size(), MaxConfigFileSize)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	if f.Version != SupportedVersion {
		return nil, fmt.Errorf(
			"config file %s: unsupported version %d (this build supports version %d)",
			path, f.Version, SupportedVersion)
	}

	f.BaseDir = filepath.Dir(path)
	return &f, nil
}

// Discover looks for a config file in cwd. If none of DefaultFilenames
// exists, it returns (nil, nil) — absence is not an error, callers
// just fall through to their existing flag-driven behavior.
//
// Only cwd is checked; there is no walk-up to parent directories.
// This matches the issue spec ("from CWD") and avoids surprises
// where a config two levels up silently changes a command's behavior.
func Discover(cwd string) (*File, error) {
	for _, name := range DefaultFilenames {
		path := filepath.Join(cwd, name)
		_, err := os.Stat(path)
		if err == nil {
			return Load(path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat candidate config %s: %w", path, err)
		}
	}
	return nil, nil
}

// ExpandSchema returns the concrete file paths matched by the pair's
// Schema globs, resolved against baseDir. Patterns may use doublestar
// syntax (`**` for recursive matches). Results are deduplicated and
// sorted for stable downstream behavior.
//
// An empty pattern list returns an empty slice (no error). A pattern
// that matches nothing also produces no error here — the caller
// decides whether zero matches is meaningful (validate, for instance,
// treats a pair with no query matches as a no-op rather than a
// failure).
func (p SQLPair) ExpandSchema(baseDir string) ([]string, error) {
	return expandGlobs(baseDir, p.Schema)
}

// ExpandQueries is the queries-side counterpart to ExpandSchema; see
// that method for the semantics of pattern matching, deduplication,
// and zero matches.
func (p SQLPair) ExpandQueries(baseDir string) ([]string, error) {
	return expandGlobs(baseDir, p.Queries)
}

// expandGlobs resolves each pattern against baseDir using doublestar
// (so `**` patterns work) and returns a sorted, deduplicated list of
// matched paths. Absolute patterns are honored as-is; baseDir is only
// joined for relative patterns.
//
// Why not filepath.Glob: stdlib Glob does not support `**`, which the
// YAML schema explicitly relies on (`queries/**/*.sql`). Doublestar
// is the de-facto Go library for shell-style recursive globs.
func expandGlobs(baseDir string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var out []string
	for _, pat := range patterns {
		full := pat
		if !filepath.IsAbs(pat) {
			full = filepath.Join(baseDir, pat)
		}
		matches, err := doublestar.FilepathGlob(full)
		if err != nil {
			return nil, fmt.Errorf("expand glob %q: %w", pat, err)
		}
		for _, m := range matches {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}
