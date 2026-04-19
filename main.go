// Command sql-ai-tools is the CLI / MCP server entry point for the
// agent-friendly CockroachDB SQL tooling described in the project README.
package main

import (
	"fmt"
	"log"

	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/parser"
	"github.com/cockroachdb/cockroachdb-parser/pkg/sql/sem/tree"
)

func main() {
	stmt, err := parser.ParseOne("SELECT 1")
	if err != nil {
		log.Fatal(err)
	}
	cfg := tree.DefaultPrettyCfg()
	pretty, err := cfg.Pretty(stmt.AST)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(pretty)
}
