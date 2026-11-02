//go:build b6_wall_timeout

package taskengine

import "testing"

// Mandatory separate gate: production's real ten-second deadline remains unchanged.
func TestB6ProviderWallTimeout(t *testing.T) { b6ProviderOffline(t, true) }
