//go:build plant_new_reference

package outbox

// Planted bug: every submission attempt uses a new exchange reference.
const (
	plantedNewReference       = true
	plantedTimeoutIsRejection = false
)
