// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package catalog provides a lightweight in-memory representation of a
// SQL schema loaded from CREATE TABLE DDL. The primary entry point is
// Load, which accepts a mix of file paths and raw SQL strings via
// SchemaSource. LoadFiles is a convenience wrapper for file-only
// callers.
//
// The catalog is the foundation for Tier 2 (schema-aware) analysis:
// name resolution, column type lookup, and "did you mean?" suggestions
// all read from this structure. It is built once, then read-only — no
// concurrent access is expected, so no synchronization is needed.
package catalog

import "strings"

// Catalog is the in-memory representation of a SQL schema loaded from
// one or more DDL files. It holds an ordered list of tables (preserving
// file order for deterministic output) and a case-insensitive name
// index for O(1) lookup.
//
// Lifecycle: built once by LoadFiles, then read-only.
type Catalog struct {
	tables   []Table
	byName   map[string]int // lowercased name → index in tables
	warnings []string
}

// Table returns the table definition with the given name, or false if
// the catalog contains no such table. Lookup is case-insensitive.
func (c *Catalog) Table(name string) (Table, bool) {
	i, ok := c.byName[strings.ToLower(name)]
	if !ok {
		return Table{}, false
	}
	return c.tables[i], true
}

// Warnings returns any non-fatal issues encountered during loading,
// such as skipped statement types or duplicate table definitions. The
// catalog is still usable when warnings are present.
func (c *Catalog) Warnings() []string {
	return c.warnings
}

// TableNames returns the names of all loaded tables in the order they
// were encountered across the input files.
func (c *Catalog) TableNames() []string {
	names := make([]string, len(c.tables))
	for i, t := range c.tables {
		names[i] = t.Name
	}
	return names
}

// Table is the parsed metadata for a single CREATE TABLE statement.
type Table struct {
	Name       string   `json:"name"`
	Columns    []Column `json:"columns"`
	PrimaryKey []string `json:"primary_key"`
	Indexes    []Index  `json:"indexes"`
}

// Column is one column extracted from a CREATE TABLE definition.
type Column struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default,omitempty"`
}

// Index is a secondary index (including UNIQUE constraints) on a table.
type Index struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
}
