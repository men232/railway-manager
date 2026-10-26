// Package view holds the memoized selectors that screens render from: pure
// functions from the store and change set to rows, with the change-set
// overlay applied.
package view

import (
	"strconv"
	"strings"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/store"
)

// Cache remembers the last result of one selector. Get recomputes only when
// key differs from the previous call's key.
type Cache[R any] struct {
	key string
	val R
	ok  bool
}

// Get returns the cached value for key, or computes and remembers it.
func (c *Cache[R]) Get(key string, compute func() R) R {
	if c.ok && c.key == key {
		return c.val
	}
	c.val, c.key, c.ok = compute(), key, true
	return c.val
}

// key joins parts into a cache key. Parts never contain NUL.
func key(parts ...string) string { return strings.Join(parts, "\x00") }

func rev(s *store.Store, cols ...store.Collection) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = strconv.FormatUint(s.Rev(c), 10)
	}
	return out
}

func csRev(cs *changeset.ChangeSet) string {
	if cs == nil {
		return "-"
	}
	return strconv.FormatUint(cs.Rev, 10)
}

func changes(cs *changeset.ChangeSet) []changeset.Change {
	if cs == nil {
		return nil
	}
	return cs.Changes
}
