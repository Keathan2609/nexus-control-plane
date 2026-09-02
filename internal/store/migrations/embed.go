// Package migrations embeds the SQL schema migrations into the binary.
//
// Embedding rather than shipping a migrations directory alongside the binary means a
// container image cannot be built with a binary and a mismatched schema: the migrations
// a build will apply are exactly the ones it was compiled from.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
