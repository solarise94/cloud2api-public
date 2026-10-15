package loginbridge

import "sync"

// reauthLocks serializes re-authorization per storage id across the process:
// the re-read, update, restore and verification of one storage form a single
// critical section, so a rollback of one save can never interleave with (and
// undo) another save of the same storage.
var reauthLocks struct {
	sync.Mutex
	holders map[uint]*sync.Mutex
}

func lockStorage(id uint) func() {
	reauthLocks.Lock()
	mu, ok := reauthLocks.holders[id]
	if !ok {
		mu = &sync.Mutex{}
		if reauthLocks.holders == nil {
			reauthLocks.holders = make(map[uint]*sync.Mutex)
		}
		reauthLocks.holders[id] = mu
	}
	reauthLocks.Unlock()
	mu.Lock()
	return mu.Unlock
}
