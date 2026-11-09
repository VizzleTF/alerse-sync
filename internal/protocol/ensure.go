package protocol

import (
	"context"
	"errors"
	"time"
)

// ErrFull is returned by EnsureChain when the server keeps MaxChains chains.
var ErrFull = errFull

// WaitError is returned by EnsureChain when the client's address may not
// create another chain yet.
type WaitError struct{ Wait time.Duration }

func (e WaitError) Error() string { return "too many new sync chains" }

// EnsureChain creates the chain as its first GetUpdates would, with its
// permanent entities, if it does not exist yet. The same limits apply: the
// create gate and MaxChains. Other parts of the server that keep data of a
// chain, such as its sign-in, call it before they store anything.
func (s *Service) EnsureChain(ctx context.Context, chainID string) error {
	_, err := s.chain(ctx, chainID)
	var wait throttled
	if errors.As(err, &wait) {
		return WaitError{wait.wait}
	}
	return err
}
