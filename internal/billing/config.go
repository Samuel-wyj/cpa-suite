package billing

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultStateFile = "plugins/cpa-key-billing-state-v1.db"

type Config struct {
	Enabled               bool   `yaml:"enabled"`
	Debug                 bool   `yaml:"debug"`
	StateFile             string `yaml:"state_file"`
	CodexFastModeBilling  bool   `yaml:"codex_fast_mode_billing"`
	MaskAPIKeyViewEmails  bool   `yaml:"mask_api_key_view_emails"`
	AllowAPIKeyQuotaReset bool   `yaml:"allow_api_key_quota_reset"`
	// ConcurrencyStaleMinutes reclaims concurrency slots with no lifecycle
	// activity for this many minutes; 0 disables reclamation.
	ConcurrencyStaleMinutes int `yaml:"concurrency_stale_minutes"`
	// RouteSessionTTLMinutes keeps a session on the credential the plugin
	// picked for a routing rule; 0 disables plugin stickiness.
	RouteSessionTTLMinutes int `yaml:"route_session_ttl_minutes"`
}

// DefaultRouteSessionTTLMinutes matches a CPA session-affinity-ttl of 2h.
const DefaultRouteSessionTTLMinutes = 120

// MaxRouteSessionTTLMinutes bounds the configurable stickiness (one day).
const MaxRouteSessionTTLMinutes = 1440

func DefaultConfig() Config {
	return Config{
		Enabled:                 false,
		StateFile:               DefaultStateFile,
		ConcurrencyStaleMinutes: DefaultConcurrencyStaleMinutes,
		RouteSessionTTLMinutes:  DefaultRouteSessionTTLMinutes,
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		document := struct {
			Config `yaml:",inline"`
			// These fields belong to the host and are ignored by the plugin.
			Priority int       `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&document); errDecode != nil {
			return Config{}, fmt.Errorf("Parse plugin configuration: %w", errDecode)
		}
		if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
			return Config{}, fmt.Errorf("Plugin configuration must contain exactly one YAML document")
		}
		cfg = document.Config
	}
	return cfg.normalized(), nil
}

func (c Config) describe() string {
	if c.Enabled {
		return "enabled"
	}
	return "disabled"
}

func (c Config) normalized() Config {
	c.StateFile = strings.TrimSpace(c.StateFile)
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	if c.ConcurrencyStaleMinutes < 0 {
		c.ConcurrencyStaleMinutes = 0
	}
	if c.ConcurrencyStaleMinutes > MaxConcurrencyStaleMinutes {
		c.ConcurrencyStaleMinutes = MaxConcurrencyStaleMinutes
	}
	c.RouteSessionTTLMinutes = min(max(c.RouteSessionTTLMinutes, 0), MaxRouteSessionTTLMinutes)
	return c
}
