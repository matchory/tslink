package core

import "sync"

// StateClaims records which endpoint uses each state directory. A directory
// serves one endpoint at a time, and garbage collection leaves claimed ones
// alone. The driver creates one and passes it to its endpoints and to garbage
// collection.
type StateClaims struct {
	mu     sync.Mutex
	owners map[string]string // state directory -> endpoint ID, or gcClaim
}

// NewStateClaims returns a registry without claims.
func NewStateClaims() *StateClaims {
	return &StateClaims{owners: make(map[string]string)}
}

// claim records owner as the user of dir, unless another owner holds it: then
// it returns that owner and false.
func (c *StateClaims) claim(dir, owner string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.owners[dir]; ok && held != owner {
		return held, false
	}
	c.owners[dir] = owner
	return owner, true
}

// claimFree records owner as the user of dir unless anyone holds it.
func (c *StateClaims) claimFree(dir, owner string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.owners[dir]; ok {
		return false
	}
	c.owners[dir] = owner
	return true
}

// release gives up owner's claim on dir. A nil registry holds no claims.
func (c *StateClaims) release(dir, owner string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owners[dir] == owner {
		delete(c.owners, dir)
	}
}
