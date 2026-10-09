//go:build !plant_new_reference && !plant_timeout_rejection

package outbox

// Planted bugs prove that the chaos harness catches the mistakes this design avoids.
// They exist only in builds with a plant_* tag; see README.md, "The harness catches the
// bugs it is built to catch". In a normal build both are false constants, and the compiler
// removes the branches that read them.
const (
	plantedNewReference       = false
	plantedTimeoutIsRejection = false
)
