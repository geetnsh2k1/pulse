package cli

import (
	"path/filepath"

	"pulse/internal/config"
)

func workDir() string {
	if flagChdir != "" {
		return flagChdir
	}
	return "."
}

// loadProject finds pulse.yaml from the working dir upward and loads it
// through strict validation.
func loadProject() (*config.Config, error) {
	path, err := config.Find(workDir())
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// findRoot locates the project root without requiring a valid config —
// commands like `pulse stop` must work even while pulse.yaml is broken.
func findRoot() (string, error) {
	path, err := config.Find(workDir())
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}
