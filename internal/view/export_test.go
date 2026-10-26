package view

// MonitorBuilds is how many times Monitor built its rows.
func (v *Views) MonitorBuilds() int { return v.monitorBuilds }

// TreeBuilds is how many times Tree built its groups.
func (v *Views) TreeBuilds() int { return v.treeBuilds }

// FeedBuilds is how many times Feed built its rows.
func (v *Views) FeedBuilds() int { return v.feedBuilds }

// RotateBuilds is how many times Rotate built its rows.
func (v *Views) RotateBuilds() int { return v.rotateBuilds }

// StalledBuilds is how many times Stalled built its list.
func (v *Views) StalledBuilds() int { return v.stalledBuilds }
