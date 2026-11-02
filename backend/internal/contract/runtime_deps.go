//go:build tools

// This file keeps the oapi-codegen generated union helpers' runtime dependency
// pinned in backend/go.mod even before the generated types file exists. It is
// excluded from normal builds; generated code imports this module directly.
package contract

import _ "github.com/oapi-codegen/runtime"
