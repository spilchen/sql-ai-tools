// This is a stub go.mod that prevents the main sql-ai-tools module
// from trying to compile this directory. The extractor imports the
// full cockroach repo and must be built inside that module context
// (see Makefile for usage).
module github.com/spilchen/sql-ai-tools/cmd/extract-builtins

go 1.26.2
