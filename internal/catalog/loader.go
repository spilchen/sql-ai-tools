// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package catalog

import (
	"fmt"
	"os"
	"strings"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

// MaxSchemaFileSize is the largest schema source Load will accept,
// whether from a file or raw SQL. Schema DDL is typically small; a
// 100 MB limit prevents accidental OOM from passing a full database
// dump.
const MaxSchemaFileSize = 100 << 20

// SchemaSource represents one source of schema DDL. Either Path or SQL
// must be set (not both). Label is used in warnings and error messages;
// when empty it defaults to Path (for file sources) or "<inline SQL>"
// (for raw SQL sources).
type SchemaSource struct {
	Path  string // file to read
	SQL   string // raw SQL content (used when Path is empty)
	Label string // human-readable name for diagnostics
}

func (s SchemaSource) label() string {
	if s.Label != "" {
		return s.Label
	}
	if s.Path != "" {
		return s.Path
	}
	return "<inline SQL>"
}

// Load parses schema SQL from the given sources and builds a Catalog
// from the CREATE TABLE statements found. Sources may be file paths or
// raw SQL strings. Non-CREATE TABLE statements are skipped with a
// warning. If multiple sources define the same table name, the last
// definition wins and a warning is recorded.
func Load(sources []SchemaSource) (*Catalog, error) {
	cat := &Catalog{byName: make(map[string]int)}

	for _, src := range sources {
		label := src.label()

		sql, err := readSource(src)
		if err != nil {
			return nil, err
		}
		if sql == "" {
			cat.warnings = append(cat.warnings,
				fmt.Sprintf("%s: source contained no SQL statements", label))
			continue
		}

		stmts, err := parser.Parse(sql)
		if err != nil {
			return nil, fmt.Errorf("parse schema %s: %w", label, err)
		}

		var skippedTags []string
		for _, stmt := range stmts {
			ct, ok := stmt.AST.(*tree.CreateTable)
			if !ok || ct.As() {
				skippedTags = append(skippedTags, stmt.AST.StatementTag())
				continue
			}
			tableName := ct.Table.Table()
			if tableName == "" {
				cat.warnings = append(cat.warnings,
					fmt.Sprintf("%s: skipped CREATE TABLE with empty table name", label))
				continue
			}
			tbl := extractTable(ct)
			key := strings.ToLower(tbl.Name)
			if idx, exists := cat.byName[key]; exists {
				cat.warnings = append(cat.warnings,
					fmt.Sprintf("%s: table %q defined more than once; using last definition", label, tbl.Name))
				cat.tables[idx] = tbl
			} else {
				cat.byName[key] = len(cat.tables)
				cat.tables = append(cat.tables, tbl)
			}
		}

		if len(skippedTags) > 0 {
			cat.warnings = append(cat.warnings,
				fmt.Sprintf("%s: skipped %d non-CREATE TABLE statement(s): %s",
					label, len(skippedTags), strings.Join(skippedTags, ", ")))
		}
	}

	return cat, nil
}

// LoadFiles parses the SQL content of each file and builds a Catalog.
// It is a convenience wrapper around Load for callers that only have
// file paths.
func LoadFiles(paths []string) (*Catalog, error) {
	sources := make([]SchemaSource, len(paths))
	for i, p := range paths {
		sources[i] = SchemaSource{Path: p}
	}
	return Load(sources)
}

// readSource returns the SQL content for a single SchemaSource. For
// file-based sources it validates the file size before reading.
func readSource(src SchemaSource) (string, error) {
	if src.Path != "" && src.SQL != "" {
		return "", fmt.Errorf("schema source %s has both Path and SQL set; only one is allowed",
			src.label())
	}
	if src.Path == "" {
		return src.SQL, nil
	}

	label := src.label()

	info, err := os.Stat(src.Path)
	if err != nil {
		return "", fmt.Errorf("stat schema file %s: %w", label, err)
	}
	if info.Size() > MaxSchemaFileSize {
		return "", fmt.Errorf("schema file %s is too large (%d bytes, max %d)",
			label, info.Size(), MaxSchemaFileSize)
	}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return "", fmt.Errorf("read schema file %s: %w", label, err)
	}
	return string(data), nil
}

// extractTable converts a parsed CREATE TABLE AST into the catalog's
// Table representation. It handles two known parser edge cases:
//
//  1. A column with an inline PRIMARY KEY constraint has
//     PrimaryKey.IsPrimaryKey set but Nullability left at SilentNull.
//     The loader forces such columns to not-null.
//
//  2. A column with an inline UNIQUE constraint has no synthesized
//     index name. The loader generates one as <table>_<column>_key,
//     matching CockroachDB's naming convention.
func extractTable(ct *tree.CreateTable) Table {
	tableName := ct.Table.Table()

	var columns []Column
	var pkCols []string
	var indexes []Index

	for _, def := range ct.Defs {
		switch d := def.(type) {
		case *tree.ColumnTableDef:
			col := extractColumn(d)
			columns = append(columns, col)

			if d.PrimaryKey.IsPrimaryKey {
				pkCols = append(pkCols, col.Name)
			}

			if d.Unique.IsUnique && !d.Unique.WithoutIndex {
				name := string(d.Unique.ConstraintName)
				if name == "" {
					name = tableName + "_" + col.Name + "_key"
				}
				indexes = append(indexes, Index{
					Name:    name,
					Columns: []string{col.Name},
					Unique:  true,
				})
			}

		case *tree.UniqueConstraintTableDef:
			cols := indexElemColumns(d.Columns)
			if d.PrimaryKey {
				pkCols = cols
			} else {
				indexes = append(indexes, Index{
					Name:    string(d.Name),
					Columns: cols,
					Unique:  true,
				})
			}

		case *tree.IndexTableDef:
			indexes = append(indexes, Index{
				Name:    string(d.Name),
				Columns: indexElemColumns(d.Columns),
				Unique:  false,
			})
		}
	}

	// PK columns are implicitly NOT NULL in SQL. Inline PKs are
	// handled in extractColumn, but table-level PRIMARY KEY (a, b)
	// constraints are resolved after all columns are collected.
	if len(pkCols) > 0 {
		pkSet := make(map[string]struct{}, len(pkCols))
		for _, pk := range pkCols {
			pkSet[pk] = struct{}{}
		}
		for i := range columns {
			if _, ok := pkSet[columns[i].Name]; ok {
				columns[i].Nullable = false
			}
		}
	}

	if columns == nil {
		columns = []Column{}
	}
	if pkCols == nil {
		pkCols = []string{}
	}
	if indexes == nil {
		indexes = []Index{}
	}

	return Table{
		Name:       tableName,
		Columns:    columns,
		PrimaryKey: pkCols,
		Indexes:    indexes,
	}
}

func extractColumn(col *tree.ColumnTableDef) Column {
	nullable := !col.PrimaryKey.IsPrimaryKey && col.Nullable.Nullability != tree.NotNull

	c := Column{
		Name:     string(col.Name),
		Type:     col.Type.SQLString(),
		Nullable: nullable,
	}

	if col.HasDefaultExpr() {
		s := tree.AsString(col.DefaultExpr.Expr)
		c.Default = &s
	}

	return c
}

func indexElemColumns(elems tree.IndexElemList) []string {
	cols := make([]string, len(elems))
	for i, elem := range elems {
		cols[i] = string(elem.Column)
	}
	return cols
}
