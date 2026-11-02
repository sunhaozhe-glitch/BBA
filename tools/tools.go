//go:build tools

// Package tools pins the oapi-codegen version used by tools/prefetch.sh.
// It is never imported by application code.
package tools

import _ "github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
