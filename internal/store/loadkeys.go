package store

import (
	"strconv"
	"time"

	"github.com/men232/railway-manager/internal/domain"
)

// Load status keys. They are pure string building, kept here with the load
// status they index so that readers need not import the loaders.
const CatalogKey = "catalog"

// InventoryKey is the load status key of a project's inventory.
func InventoryKey(projectID string) string { return "inventory:" + projectID }

// Load status keys of the history reads.
func HistoryKey(envID string) string     { return "history:" + envID }
func MembersKey(projectID string) string { return "members:" + projectID }
func PatchesKey(envID string) string     { return "patches:" + envID }

// Load status keys of the monitor reads.
func StatesKey(projectID string) string  { return "states:" + projectID }
func MetricsKey(projectID string) string { return "metrics:" + projectID }

// PlaceStatesKey is the load status key of a states read made for one place
// (the Logs screen), apart from the project-wide StatesKey.
func PlaceStatesKey(p domain.Place) string { return "states-of:" + p.Key() }
func DetailKey(p domain.Place) string      { return "detail:" + p.Key() }
func ConfigKey(envID string) string        { return "config:" + envID }

// PanelKey is the load status key of a place's panel metrics read at step
// seconds over span.
func PanelKey(p domain.Place, step int, span time.Duration) string {
	return "panel:" + p.Key() + "/" + strconv.Itoa(step) + "s/" + strconv.FormatInt(int64(span/time.Second), 10) + "s"
}
