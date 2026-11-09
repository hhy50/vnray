package access

import (
	"context"
	"errors"
	"sync"

	"github.com/hhy50/vnray/internal/authkey"
)

var errTransportKeyNotRegistered = errors.New("transport key not registered")

type authKeyEntry struct {
	key   authkey.Key
	scope string
}

type authKeyIndex struct {
	mu     sync.RWMutex
	loaded bool
	byID   map[authkey.ID]authKeyEntry
	// Invalid legacy rows still disable configuration-file fallback.
	rows map[string]struct{}
}

func (index *authKeyIndex) invalidate() {
	index.loaded = false
	index.byID = nil
	index.rows = nil
}

// ReloadAuthKeys replaces the index from storage, rejecting authentication on failure.
func (b *Bindings) ReloadAuthKeys(ctx context.Context) error {
	b.authKeys.mu.Lock()
	defer b.authKeys.mu.Unlock()
	return b.loadAuthKeysLocked(ctx)
}

func (b *Bindings) loadAuthKeysLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	keys, err := b.store.ListAuthKeys(ctx)
	if err != nil {
		b.authKeys.invalidate()
		return err
	}
	byID := make(map[authkey.ID]authKeyEntry, len(keys))
	rows := make(map[string]struct{}, len(keys))
	for _, entry := range keys {
		rows[entry.Key] = struct{}{}
		key, err := authkey.Parse(entry.Key)
		if err == nil {
			byID[key.ID()] = authKeyEntry{key: key, scope: entry.Scope}
		}
	}
	b.authKeys.byID = byID
	b.authKeys.rows = rows
	b.authKeys.loaded = true
	return nil
}

// lockAuthKeys returns with RLock held on success, including after a cold load.
func (b *Bindings) lockAuthKeys(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.authKeys.mu.RLock()
		if b.authKeys.loaded {
			return nil
		}
		b.authKeys.mu.RUnlock()
		b.authKeys.mu.Lock()
		var err error
		if !b.authKeys.loaded {
			err = b.loadAuthKeysLocked(ctx)
		}
		b.authKeys.mu.Unlock()
		if err != nil {
			return err
		}
	}
}
