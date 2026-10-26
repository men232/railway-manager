package monitor

// Columns are the widths, in terminal cells, of the services table.
type Columns struct {
	Project, Env, Name, Status, Chart int
	StatusIconOnly                    bool
}

const (
	gap          = 2
	statusFull   = 20
	statusIcon   = 1
	valueCPU     = 7
	valueMem     = 11
	valueTraffic = 13
	values       = valueCPU + valueMem + valueTraffic
	keepChart    = 8
)

// Layout sizes the table for a terminal width. maxProject, maxEnv and maxName
// are the longest contents (in cells); each column takes its content, at least
// its title, capped at 16, 12 and 28. Charts never squeeze these columns: they
// share what is left with the names whole, the status showing its icon only
// when its label would leave them fewer than 8 cells each. When even that does
// not fit, there are no charts: name, project and env start from 14, 7 and 3,
// and the room left after the values goes to env (up to its content), then the
// status label, then name and project.
func Layout(width int, maxProject, maxEnv, maxName int) Columns {
	full := [3]int{ // project, env, name
		min(16, max(7, maxProject)),
		min(12, max(3, maxEnv)),
		min(28, max(4, maxName)),
	}
	w := full
	room := func(status int) int { return width - status - gap*6 - w[0] - w[1] - w[2] }
	wanted := (keepChart+1)*3 + values
	status := statusIcon
	if room(statusFull) >= wanted {
		status = statusFull
	}
	if chart := (room(status) - values - 3) / 3; chart >= keepChart {
		return Columns{Project: w[0], Env: w[1], Name: w[2], Status: status, Chart: chart, StatusIconOnly: status == statusIcon}
	}
	mins := [3]int{7, 3, 14}
	for i := range w {
		w[i] = min(mins[i], full[i])
	}
	status = statusIcon
	free := room(status) - values
	grow := func(i int) {
		n := max(0, min(free, full[i]-w[i]))
		w[i] += n
		free -= n
	}
	grow(1)
	if free >= statusFull-statusIcon {
		status = statusFull
		free -= statusFull - statusIcon
	}
	grow(2)
	grow(0)
	return Columns{Project: w[0], Env: w[1], Name: w[2], Status: status, StatusIconOnly: status == statusIcon}
}
