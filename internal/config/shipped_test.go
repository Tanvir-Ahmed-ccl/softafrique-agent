package config

import "testing"

// The shipped example and the installer template must survive strict parsing.
// A fresh install or a copy-pasted example that fails at startup is the worst
// possible outcome for a change made to catch exactly that class of mistake.
func TestShippedConfigsLoad(t *testing.T) {
	for _, path := range []string{
		"../../config.example.yaml",
		"../../installer/config.yaml.template",
	} {
		cfg := Default()
		if err := loadYAML(readFile(t, path), cfg); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: validate: %v", path, err)
		}
	}
}
