package history

import "github.com/men232/railway-manager/internal/loaders"

// Page is the history page a pageMsg carries, for the tests.
func (m pageMsg) Page() loaders.HistoryMsg { return m.HistoryMsg }
