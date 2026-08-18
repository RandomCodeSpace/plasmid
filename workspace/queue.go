package workspace

import "context"

// MutationQueue serializes all workspace mutations.
type MutationQueue struct {
	sem chan struct{}
}

// NewMutationQueue creates a queue with one global mutation slot.
func NewMutationQueue() *MutationQueue {
	return &MutationQueue{sem: make(chan struct{}, 1)}
}

// Do waits for the mutation slot or context cancellation, then invokes fn.
func (q *MutationQueue) Do(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case q.sem <- struct{}{}:
	default:
		select {
		case q.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-q.sem }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}
