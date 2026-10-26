// Package domain holds the pure helpers shared by the store and change set:
// places, variable references, and value fingerprints.
package domain

// Place is a scope inside a project: one environment, and either one service
// or the shared scope of that environment (ServiceID == "").
type Place struct {
	ProjectID string
	EnvID     string
	ServiceID string
}

// Shared reports whether the place is the shared scope of its environment.
func (p Place) Shared() bool {
	return p.ServiceID == ""
}

// Key identifies the place as "envID/serviceID", or "envID/shared" for the shared scope.
func (p Place) Key() string {
	if p.Shared() {
		return p.EnvID + "/shared"
	}
	return p.EnvID + "/" + p.ServiceID
}

// LocationID identifies a named variable within a place.
func LocationID(envID, serviceID, name string) string {
	return Place{EnvID: envID, ServiceID: serviceID}.Key() + "/" + name
}
