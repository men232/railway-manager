package tui

// Icons returns glyphs for states. The plain set renders everywhere; Nerd
// swaps in Nerd Font glyphs where they exist.
type Icons struct{ Nerd bool }

func (i Icons) pick(plain, nerd string) string {
	if i.Nerd {
		return nerd
	}
	return plain
}

// Gap goes between an icon and its text: a Nerd Font glyph draws wider
// than its cell, so it gets a second space.
func (i Icons) Gap() string { return i.pick(" ", "  ") }

func (i Icons) Ok() string      { return i.pick("✓", "") }
func (i Icons) Fail() string    { return i.pick("✗", "") }
func (i Icons) Warn() string    { return "!" }
func (i Icons) Pending() string { return "●" }
func (i Icons) Added() string   { return "+" }
func (i Icons) Edit() string    { return i.pick("✎", "") }
func (i Icons) Run() string     { return "⟳" }
func (i Icons) Sep() string     { return " · " }
func (i Icons) Blocked() string { return "⊘" }
func (i Icons) Skipped() string { return "»" }
func (i Icons) Todo() string    { return "○" }

// RunMark marks a history entry applied by a railway-manager run.
func (i Icons) RunMark() string { return i.pick("◆", "\uf219") }

// Drift marks a pending change whose base changed on Railway.
func (i Icons) Drift() string { return i.pick("⚠", "\uf071") }

// Status maps a monitor status glyph (standard Unicode) to its Nerd form;
// a glyph without one is returned as is. The "reading" glyph "…" would read
// as a cut cell, so it shows as "◇" (Nerd: an hourglass).
func (i Icons) Status(icon string) string {
	if icon == "…" {
		return i.pick("◇", "\uf252")
	}
	if !i.Nerd {
		return icon
	}
	switch icon {
	case "●":
		return "\uf111"
	case "◐":
		return "\uf042"
	case "◌":
		return "\uf110"
	case "✗":
		return i.Fail()
	case "○":
		return "\uf10c"
	case "☾":
		return "\uf186"
	}
	return icon
}

// Glyphs of the services table's value columns: CPU, memory, traffic.
func (i Icons) CPU() string     { return i.pick("▤", "\U000f035b") }
func (i Icons) Memory() string  { return i.pick("▥", "\U000f08ae") }
func (i Icons) Traffic() string { return i.pick("⇅", "\U000f04e2") }

// Glyphs of the services panel. A plain form that is "" means the item is
// shown without an icon.
func (i Icons) Region() string        { return i.pick("◉", "\U000f01e7") }
func (i Icons) Repo() string          { return i.pick("", "\U000f02a4") }
func (i Icons) Branch() string        { return i.pick("⎇", "\U000f062c") }
func (i Icons) Builder() string       { return i.pick("⚙", "\U000f1323") }
func (i Icons) Restart() string       { return i.pick("↻", "\U000f0709") }
func (i Icons) Sleep() string         { return i.pick("☾", "\U000f04b2") }
func (i Icons) Health() string        { return i.pick("♥", "\U000f05f6") }
func (i Icons) Commit() string        { return i.pick("●", "\U000f0718") }
func (i Icons) Globe() string         { return i.pick("⊕", "\U000f059f") }
func (i Icons) Disk() string          { return i.pick("⛁", "\U000f02ca") }
func (i Icons) DeployOK() string      { return i.pick("✓", "\U000f012c") }
func (i Icons) DeployFailed() string  { return i.pick("✗", "\U000f0159") }
func (i Icons) DeployRunning() string { return i.pick("◌", "\U000f0996") }

// Glyphs of the Settings screen: its sections and settings.
func (i Icons) Account() string   { return i.pick("@", "") }
func (i Icons) Projects() string  { return i.pick("▦", "") }
func (i Icons) Envs() string      { return i.pick("≡", "") }
func (i Icons) Writable() string  { return i.pick("✎", "") }
func (i Icons) LeakTime() string  { return i.pick("◷", "") }
func (i Icons) Names() string     { return i.pick("#", "") }
func (i Icons) Glyphs() string    { return i.pick("◆", "") }
func (i Icons) Token() string     { return i.pick("◇", "") }
func (i Icons) LastRead() string  { return i.pick("↻", "") }
func (i Icons) Project() string   { return i.pick("▪", "") }
func (i Icons) EnvDot() string    { return i.pick("●", "") }
func (i Icons) Chosen() string    { return i.pick("◉", "") }
func (i Icons) NotChosen() string { return i.pick("○", "") }

// Glyphs of the Home menu.
func (i Icons) Services() string  { return i.pick("▤", "\uf233") }
func (i Icons) Variables() string { return i.pick("≡", "\uf03a") }
func (i Icons) Rotate() string    { return i.pick("↻", "\uf084") }
func (i Icons) ChangeSet() string { return i.pick("●", "\uf0ae") }
func (i Icons) History() string   { return i.pick("◷", "\uf1da") }
func (i Icons) Settings() string  { return i.pick("⚙", "\uf013") }
