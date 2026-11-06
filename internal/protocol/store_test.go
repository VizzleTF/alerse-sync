package protocol

import (
	"testing"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	"github.com/VizzleTF/arcomium-sync/internal/store/memory"
)

// newStore returns an empty store for a test: in memory, or in Postgres
// with -tags integration.
var newStore = func(*testing.T) store.Store { return memory.New() }
