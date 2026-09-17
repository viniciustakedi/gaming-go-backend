package walletapp

import (
	"fmt"

	"github.com/google/uuid"
)

// newID mints an internal identifier as UUID v7, so ids sort roughly by
// creation time without the entropy problems of a naive timestamp-prefixed
// id.
func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("walletapp: generate id: %w", err)
	}
	return id.String(), nil
}
