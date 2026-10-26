// Package settings persists user settings as JSON.
package settings

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/men232/railway-manager/internal/workdir"
)

// Settings are the persisted user preferences. Icons is "auto", "nerd" or "plain".
type Settings struct {
	Projects  []string `json:"projects"`
	Envs      []string `json:"envs"`
	WriteEnvs []string `json:"writeEnvs"`
	Internal  []string `json:"internal"`
	Since     string   `json:"since"`
	Icons     string   `json:"icons"`
}

func defaults() Settings {
	return Settings{Icons: "auto"}
}

// Load reads settings from path. A missing file yields defaults.
func Load(path string) (Settings, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	s := defaults()
	if err := json.Unmarshal(b, &s); err != nil {
		return Settings{}, err
	}
	if s.Icons == "" {
		s.Icons = "auto"
	}
	return s, nil
}

// Save atomically writes settings to path with mode 0600.
func Save(path string, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(path, append(b, '\n'), 0o600)
}
