package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig_AllowedCIDRs(t *testing.T) {
	// Setup env
	os.Setenv("ALLOWED_CIDRS", "192.168.1.0/24,10.0.0.1/32,invalid-ip")
	defer os.Unsetenv("ALLOWED_CIDRS")

	cfg := LoadConfig()

	if len(cfg.AllowedCIDRs) != 3 {
		t.Errorf("Expected 3 raw CIDR strings, got %d", len(cfg.AllowedCIDRs))
	}

	// Check AllowedCIDRNets
	if len(cfg.AllowedCIDRNets) != 2 {
		t.Errorf("Expected 2 valid IPNets, got %d", len(cfg.AllowedCIDRNets))
	}

	// Verify the parsed nets
	// 192.168.1.0/24
	if cfg.AllowedCIDRNets[0].String() != "192.168.1.0/24" {
		t.Errorf("Expected first CIDR to be 192.168.1.0/24, got %s", cfg.AllowedCIDRNets[0].String())
	}
	// 10.0.0.1/32
	if cfg.AllowedCIDRNets[1].String() != "10.0.0.1/32" {
		t.Errorf("Expected second CIDR to be 10.0.0.1/32, got %s", cfg.AllowedCIDRNets[1].String())
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	// Unset relevant env vars to test defaults
	vars := []string{"RATE_LIMIT", "CACHE_TTL_HOURS", "S3_FORCE_PATH_STYLE"}
	for _, v := range vars {
		// Store old value to restore
		oldVal, exists := os.LookupEnv(v)
		if exists {
			os.Unsetenv(v)
			defer os.Setenv(v, oldVal)
		} else {
			// Ensure it stays unset?
			// Defer nothing if it didn't exist
		}
	}

	cfg := LoadConfig()

	if cfg.RateLimit != 10 {
		t.Errorf("Expected default RateLimit 10, got %d", cfg.RateLimit)
	}

	expectedTTL := 24 * time.Hour
	if cfg.CacheTTL != expectedTTL {
		t.Errorf("Expected default CacheTTL %v, got %v", expectedTTL, cfg.CacheTTL)
	}

	if cfg.S3ForcePathStyle != false {
		t.Errorf("Expected default S3ForcePathStyle false, got %v", cfg.S3ForcePathStyle)
	}
}
