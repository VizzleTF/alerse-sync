package account

import (
	"sync"
	"time"
)

const (
	// activeTTL is how long a device known to be active skips the database:
	// a removal on another instance takes effect within it.
	activeTTL = 30 * time.Second
	// inactiveTTL is how long a key the account does not have is refused
	// without the database. Keys are free to make, so without this every
	// made-up key would cost a query.
	inactiveTTL = 10 * time.Minute
	// maxCachedDevices bounds the cache; past it, it starts over.
	maxCachedDevices = 50_000
)

// deviceCache remembers which device keys are active, per account.
type deviceCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cachedDevice
}

type cachedDevice struct {
	active bool
	until  time.Time
}

func newDeviceCache() *deviceCache {
	return &deviceCache{now: time.Now, entries: map[string]cachedDevice{}}
}

func cacheKey(accountID string, publicKey []byte) string {
	return accountID + "\x00" + string(publicKey)
}

// get returns what is known about the device, if anything still is.
func (c *deviceCache) get(accountID string, publicKey []byte) (active, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey(accountID, publicKey)]
	if !ok || !c.now().Before(e.until) {
		return false, false
	}
	return e.active, true
}

func (c *deviceCache) put(accountID string, publicKey []byte, active bool) {
	ttl := inactiveTTL
	if active {
		ttl = activeTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCachedDevices {
		c.entries = map[string]cachedDevice{}
	}
	c.entries[cacheKey(accountID, publicKey)] = cachedDevice{active: active, until: c.now().Add(ttl)}
}

// forget drops what is known about one device of the account, or about all
// of them if publicKey is nil.
func (c *deviceCache) forget(accountID string, publicKey []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if publicKey != nil {
		delete(c.entries, cacheKey(accountID, publicKey))
		return
	}
	prefix := accountID + "\x00"
	for key := range c.entries {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(c.entries, key)
		}
	}
}

// chainTTL is how long a chain's owner is known without the database. An
// owner changes only when the chain is deleted, and chain IDs come from keys
// only the owner has.
const chainTTL = 10 * time.Minute

type cachedChain struct {
	owner string
	until time.Time
}

// chainCache remembers whose chains are whose.
type chainCache struct {
	now func() time.Time

	mu     sync.Mutex
	owners map[string]cachedChain
}

func newChainCache() *chainCache {
	return &chainCache{now: time.Now, owners: map[string]cachedChain{}}
}

func (c *chainCache) get(chainID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.owners[chainID]
	if !ok || !c.now().Before(e.until) {
		return "", false
	}
	return e.owner, true
}

func (c *chainCache) put(chainID, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.owners) >= maxCachedDevices {
		c.owners = map[string]cachedChain{}
	}
	c.owners[chainID] = cachedChain{owner: owner, until: c.now().Add(chainTTL)}
}

// forgetAccount drops the chains of an account.
func (c *chainCache) forgetAccount(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for chainID, e := range c.owners {
		if e.owner == accountID {
			delete(c.owners, chainID)
		}
	}
}

// accountTTL is how long an account stays known after a device of it was
// last seen active.
const accountTTL = 24 * time.Hour

// accountSet remembers accounts with active devices. An account ID comes
// from the account's secret key and is never published, so a token naming an
// account the server has not seen is almost always made up.
type accountSet struct {
	now func() time.Time

	mu    sync.Mutex
	until map[string]time.Time
}

func newAccountSet() *accountSet {
	return &accountSet{now: time.Now, until: map[string]time.Time{}}
}

func (s *accountSet) has(accountID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.until[accountID]
	return ok && s.now().Before(until)
}

func (s *accountSet) add(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.until) >= maxCachedDevices {
		now := s.now()
		for id, until := range s.until {
			if !now.Before(until) {
				delete(s.until, id)
			}
		}
	}
	s.until[accountID] = s.now().Add(accountTTL)
}

func (s *accountSet) remove(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.until, accountID)
}
