package model

import "time"

// Spend is what one round spent on one limit, with the upstream's own
// account-wide view when it reported one.
type Spend struct {
	Limit     string    `json:"limit"`
	N         float64   `json:"n"`
	Remaining int       `json:"remaining"`         // -1 = not reported
	ResetAt   time.Time `json:"reset_at,omitzero"` // zero = not reported
}
