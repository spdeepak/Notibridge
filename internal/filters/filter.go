// Package filters decides which discovered notifications are eligible for
// forwarding. The MVP supports a single rule: a list of excluded application
// bundle identifiers, each optionally ending in ".*" to match a prefix.
package filters

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/spdeepak/notibridge/internal/notifications"
)

// Filter is an immutable, goroutine-safe exclusion filter. Build a new Filter
// whenever the configuration changes.
type Filter struct {
	mu       sync.RWMutex
	patterns []string
}

// New builds a filter from a list of bundle identifiers. Entries are matched
// case-insensitively; an entry ending in ".*" matches any bundle identifier
// with that prefix (for example "com.apple.*").
func New(exclusions []string) *Filter {
	f := &Filter{}
	for _, e := range exclusions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			f.patterns = append(f.patterns, e)
		}
	}
	return f
}

// Excluded reports whether a notification from the given bundle identifier is
// excluded from forwarding.
func (f *Filter) Excluded(appID string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	app := strings.ToLower(strings.TrimSpace(appID))
	for _, p := range f.patterns {
		if strings.HasSuffix(p, ".*") {
			if strings.HasPrefix(app, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if ok, _ := filepath.Match(p, app); ok {
			return true
		}
		if p == app {
			return true
		}
	}
	return false
}

// Match applies the filter to a whole notification.
func (f *Filter) Match(n notifications.Notification) bool {
	return !f.Excluded(n.AppID)
}
