package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// SetupFonts creates a fonts.conf file pointing to the assets/fonts directory
// and sets the FONTCONFIG_FILE environment variable.
func SetupFonts() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fontsDir := filepath.Join(cwd, "assets", "fonts")

	// Use temp dir for config and cache to avoid permission issues
	tmpDir := os.TempDir()
	fontCacheDir := filepath.Join(tmpDir, "quirm-fontconfig-cache")
	
	// Create fonts.conf
	// Minimal configuration to include the directory
	configContent := fmt.Sprintf(`<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "fonts.dtd">
<fontconfig>
  <dir>%s</dir>
  <cachedir>%s</cachedir>
</fontconfig>
`, fontsDir, fontCacheDir)

    // Ensure cache dir exists
    if err := os.MkdirAll(fontCacheDir, 0755); err != nil {
        return err
    }

	confPath := filepath.Join(tmpDir, "fonts.conf")
	if err := os.WriteFile(confPath, []byte(configContent), 0644); err != nil {
		return err
	}

	return os.Setenv("FONTCONFIG_FILE", confPath)
}
