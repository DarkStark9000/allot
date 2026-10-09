//go:build plant_timeout_rejection

package outbox

// Planted bug: a timeout is treated as a rejection, so the money is refunded.
const (
	plantedNewReference       = false
	plantedTimeoutIsRejection = true
)
