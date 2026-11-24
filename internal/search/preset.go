package search

import (
	"strings"
	"sync"
)

var (
	presetMu      sync.RWMutex
	activePresets = make(map[string]bool)

	// Preset "it": Italian energy, renewables, storage & normative accelerators (Level 3)
	presetITNationalDomains = []string{
		"cei.it", "ceinorme.it", "ceiweb.it", "static.ceinorme.it", "iec.ch",
		"pv-magazine.it", "mauromalizia.it",
	}

	presetITKeywords = map[string]bool{
		"pas": true, "au": true, "vvf": true, "dcprev": true, "pnrr": true, "cei": true,
	}

	presetITTechnicalDomains = []string{
		"cei.it", "ceinorme.it", "ceiweb.it", "static.ceinorme.it",
	}
)

// SetPresetActive activates or deactivates a named domain preset (Level 3).
func SetPresetActive(name string, active bool) {
	presetMu.Lock()
	defer presetMu.Unlock()
	name = strings.ToLower(strings.TrimSpace(name))
	if active {
		activePresets[name] = true
	} else {
		delete(activePresets, name)
	}
}

// IsPresetActive reports whether the given preset is currently active.
func IsPresetActive(name string) bool {
	presetMu.RLock()
	defer presetMu.RUnlock()
	return activePresets[strings.ToLower(strings.TrimSpace(name))]
}

// ResetPresets clears all active presets (useful for testing pure agnostic mode).
func ResetPresets() {
	presetMu.Lock()
	defer presetMu.Unlock()
	activePresets = make(map[string]bool)
}

