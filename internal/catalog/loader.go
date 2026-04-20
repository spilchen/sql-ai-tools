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

// maxSchemaFileSize is the largest schema file LoadFiles will read.
// Schema DDL files are typically small; a 100 MB limit prevents
// accidental OOM from passing a full database dump.
const maxSchemaFileSize = 100 << 20

// LoadFiles parses the SQL content of each file and builds a Catalog
// from the CREATE TABLE statements found. Non-CREATE TABLE statements
// are skipped with a warning. If multiple files define the same table
// name, the last definition wins and a warning is recorded.
func LoadFiles(paths []string) (*Catalog, error) {
	cat := &Catalog{byName: make(map[string]int)}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read schema file %s: %w", path, err)
		}
		if int64(len(data)) > maxSchemaFileSize {
			return nil, fmt.Errorf("schema file %s is too large (%d bytes, max %d)",
				path, len(data), maxSchemaFileSize)
		}

		stmts, err := parser.Parse(string(data))
		if err != nil {
			return nil, fmt.Errorf("parse schema file %s: %w", path, err)
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
				continue
			}
			tbl := extractTable(ct)
			key := strings.ToLower(tbl.Name)
			if idx, exists := cat.byName[key]; exists {
				cat.warnings = append(cat.warnings,
					fmt.Sprintf("%s: table %q defined more than once; using last definition", path, tbl.Name))
				cat.tables[idx] = tbl
			} else {
				cat.byName[key] = len(cat.tables)
				cat.tables = append(cat.tables, tbl)
			}
		}

		if len(skippedTags) > 0 {
			cat.warnings = append(cat.warnings,
				fmt.Sprintf("%s: skipped %d non-CREATE TABLE statement(s): %s",
					path, len(skippedTags), strings.Join(skippedTags, ", ")))
		}
	}

	return cat, nil
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
