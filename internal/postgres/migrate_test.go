package postgres

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestMigrateRejectsUnknownDirection(t *testing.T) {
	err := Migrate(context.Background(), nil, "sideways", io.Discard)

	if !errors.Is(err, ErrUnknownDirection) {
		t.Fatalf("error = %v, se esperaba ErrUnknownDirection", err)
	}
}
