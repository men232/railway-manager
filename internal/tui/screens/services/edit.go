package services

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// editStep is which input is open.
type editStep int

const (
	editCPU editStep = iota + 1
	editMem
	editDisk
)

// editInput is an open limits or disk size input: the limits ask for the
// cpu, then the memory.
type editInput struct {
	step    editStep
	place   domain.Place
	field   components.TextInput
	cpu     string // the cpu answer, once given
	cpuPre  string // the prefills: unchanged answers put nothing
	memPre  string
	sizePre string
	// the exact values behind the prefills: an answer left as prefilled
	// keeps its value, not the rounded text
	cpuWas   float64
	memWas   int64
	volumeID string
}

// readOnly refuses a change to a place outside the write environments, as
// Variables does: apply would refuse it, so it is not staged at all.
func readOnly(ctx *tui.Ctx, p domain.Place) tea.Cmd {
	if ctx.Settings == nil || !slices.Contains(ctx.Settings.WriteEnvs, p.EnvID) {
		return tui.Flash('✗', ctx.Store.EnvLabel(p.EnvID)+" is not in write environments · Settings w")
	}
	return nil
}

// openLimits opens the cpu input, prefilled with the effective limits: the
// override, else the limits Railway reports. Without the environment
// settings (the change is based on them) it says so at once.
func (s *screen) openLimits(ctx *tui.Ctx, r view.MonitorRow) tea.Cmd {
	cfg, ok := ctx.Store.Config(r.Place.EnvID)
	if !ok {
		return tui.Flash('!', "environment settings not loaded yet · r refresh")
	}
	e := &editInput{step: editCPU, place: r.Place}
	sc := cfg.Services[r.Place.ServiceID]
	if sc.CPU != nil {
		e.cpuWas = *sc.CPU
		e.cpuPre = trimNum(e.cpuWas)
	}
	if sc.MemoryBytes != nil {
		e.memWas = *sc.MemoryBytes
		e.memPre = memGB(e.memWas)
	}
	if e.cpuPre == "" && r.CPULimit > 0 {
		e.cpuWas = r.CPULimit
		e.cpuPre = trimNum(e.cpuWas)
	}
	if e.memPre == "" && r.MemLimitGB > 0 {
		e.memWas = int64(math.Round(r.MemLimitGB * monitor.GB))
		e.memPre = memGB(e.memWas)
	}
	e.field = components.TextInput{Label: "cpu (vCPU)"}
	e.field.SetValue(e.cpuPre)
	s.input = e
	return nil
}

// openDisk opens the size input when exactly one volume is mounted. Without
// the environment settings (the change is based on them) it says so at once.
func (s *screen) openDisk(ctx *tui.Ctx, r view.MonitorRow) tea.Cmd {
	if _, ok := ctx.Store.Config(r.Place.EnvID); !ok {
		return tui.Flash('!', "environment settings not loaded yet · r refresh")
	}
	vols := ctx.Store.Volumes(r.Place.EnvID, r.Place.ServiceID)
	switch len(vols) {
	case 0:
		return tui.Flash('!', "no volume on this service")
	case 1:
	default:
		return tui.Flash('!', "several volumes · not supported yet")
	}
	e := &editInput{step: editDisk, place: r.Place, volumeID: vols[0].VolumeID}
	e.sizePre = fmt.Sprintf("%.1f", vols[0].SizeMB/1000)
	e.field = components.TextInput{Label: "size (GB)"}
	e.field.SetValue(e.sizePre)
	s.input = e
	return nil
}

// submit takes the open input's answer: the cpu moves on to the memory; the
// memory and the size make the change.
func (s *screen) submit(ctx *tui.Ctx, value string) tea.Cmd {
	e := s.input
	value = strings.TrimSpace(value)
	switch e.step {
	case editCPU:
		e.cpu, e.step = value, editMem
		e.field = components.TextInput{Label: "memory (GB)"}
		e.field.SetValue(e.memPre)
		return nil
	case editMem:
		s.input = nil
		return s.putLimits(ctx, e, value)
	case editDisk:
		s.input = nil
		if value == e.sizePre {
			// the prefill is rounded: as is, it is the current size
			return tui.Flash('!', "size unchanged")
		}
		gb, err := number(value)
		if err != nil {
			return tui.Flash('✗', "size must be a number of GB")
		}
		c, err := kinds.ResizeVolume(ctx.Store, e.place, e.volumeID, int(math.Round(gb*1000)), ctx.Now())
		if err != nil {
			return tui.Flash('✗', err.Error())
		}
		return tui.Emit(tui.PutChangeMsg{Change: c})
	}
	s.input = nil
	return nil
}

// putLimits makes the limits change; both answers empty clear the override.
func (s *screen) putLimits(ctx *tui.Ctx, e *editInput, mem string) tea.Cmd {
	if e.cpu != "" && mem != "" && e.cpu == e.cpuPre && mem == e.memPre {
		return tui.Flash('!', "limits unchanged")
	}
	var cpu *float64
	var memBytes *int64
	switch {
	case e.cpu == "":
	case e.cpu == e.cpuPre:
		v := e.cpuWas
		cpu = &v
	default:
		v, err := number(e.cpu)
		if err != nil {
			return tui.Flash('✗', "cpu must be a number of vCPU")
		}
		cpu = &v
	}
	switch {
	case mem == "":
	case mem == e.memPre:
		b := e.memWas
		memBytes = &b
	default:
		v, err := number(mem)
		if err != nil {
			return tui.Flash('✗', "memory must be a number of GB")
		}
		b := int64(math.Round(v * monitor.GB))
		memBytes = &b
	}
	c, err := kinds.SetLimits(ctx.Store, e.place, cpu, memBytes, ctx.Now())
	if err != nil {
		return tui.Flash('✗', err.Error())
	}
	return tui.Emit(tui.PutChangeMsg{Change: c})
}

// memGB is a memory size in Railway's GB (1024³ bytes) with one decimal, as
// prefilled.
func memGB(b int64) string { return fmt.Sprintf("%.1f", float64(b)/monitor.GB) }

// maxNumber bounds what an input takes (a million GB or vCPU), so the
// conversion to bytes or MB cannot overflow.
const maxNumber = 1e6

// number parses a finite decimal number up to maxNumber.
func number(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > maxNumber {
		return 0, fmt.Errorf("not a number")
	}
	return v, nil
}
