package walletapp

import (
	"fmt"

	"github.com/google/uuid"
)

// newID mints an internal identifier as UUID v7 (spec: "Identificadores
// internos são UUID v7 gerados na aplicação"), so ids sort roughly by
// creation time without leaking any of the entropy problems a naive
// timestamp-prefixed id would have.
func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("walletapp: generate id: %w", err)
	}
	return id.String(), nil
}
