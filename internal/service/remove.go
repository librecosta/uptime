package service

import (
	"context"
	"errors"
	"slices"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

// ErrActive means the service is still in the current YAML endpoint set.
var ErrActive = errors.New("cannot remove a service still configured in endpoints")

// ErrNotFound means there is no persisted service with the requested ID.
var ErrNotFound = errors.New("service not found")

// CheckDetached must succeed before the caller opens storage for removal.
// Configuration membership does not establish that other writers have stopped.
func CheckDetached(id string, activeIDs []string) error {
	if activeSet(activeIDs)[id] {
		return ErrActive
	}
	return nil
}

// RemovalStore is the existing standalone session's list/delete boundary.
type RemovalStore interface {
	ListServices(context.Context) ([]uptimestorage.Service, error)
	RemoveService(context.Context, string) error
}

// Remove verifies existence, deletes exactly once, then checks the observed result.
// Call CheckDetached before opening store, and Close it before reporting success.
// Remove does not own the storage lifetime. Errors after a deletion attempt cannot
// guarantee that data is unchanged; no retry is performed.
// Redis writers may recreate a service even after the read-back finds it absent.
func Remove(ctx context.Context, store RemovalStore, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := store.ListServices(ctx)
	if err != nil {
		return err
	}
	matches := func(s uptimestorage.Service) bool { return s.ID == id }
	if !slices.ContainsFunc(rows, matches) {
		return ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.RemoveService(ctx, id); err != nil {
		return err
	}
	rows, err = store.ListServices(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(rows, matches) {
		return errors.New("service is still present after removal; coordinate monitoring writers")
	}
	return ctx.Err()
}
