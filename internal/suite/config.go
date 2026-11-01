// Package suite holds CPA Suite additions layered on top of the upstream
// cpa-key-billing code. Upstream files call into this package only through a
// few documented hooks (see docs/UPSTREAM_PATCHES.md), so upstream releases
// can be merged with minimal conflicts.
package suite

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Identity of the build. Version is the CPA Suite release (semver);
// UpstreamVersion is the cpa-key-billing release it is based on.
const (
	UpstreamVersion = "1.3.20"
	Version         = "1.0.0"

	// DefaultStateFile is used when the operator does not set state_file. It
	// deliberately differs from cpa-key-billing's file so the two plugins can
	// never share or corrupt one database.
	DefaultStateFile = "plugins/cpa-suite.db"
	// LegacyStateFile is cpa-key-billing's default database path.
	LegacyStateFile = "plugins/cpa-key-billing-state-v1.db"
	// DefaultPluginsDir is scanned for conflicting plugin libraries.
	DefaultPluginsDir = "plugins"
)

// Config holds the suite-only keys of plugins.configs.cpa-suite. They are
// stripped before the remaining YAML is handed to the unmodified upstream
// decoder, which rejects unknown keys.
type Config struct {
	// ImportFrom is the cpa-key-billing database to copy on first start. It
	// is opened read-only and never modified. Empty means "not configured".
	ImportFrom string
	// PluginsDir is where CPA discovers plugin libraries.
	PluginsDir string
	// Modules maps module ID to mode; see modules.go.
	Modules map[string]string
	// StateFile is the effective upstream state_file (suite default applied).
	StateFile string
	// ClientGuard is the client_guard: mapping (client-guard 0.1.2 config keys
	// plus import_from), re-encoded as YAML for the module.
	ClientGuard []byte
	// ClientGuardImportFrom is the client-guard data directory to import once;
	// empty = <plugins_dir>/client-guard, "none" = never import.
	ClientGuardImportFrom string
	// Alerts holds alerts: overrides of the alert thresholds (0.7); they
	// win over the values saved from the page.
	Alerts map[string]any
}

var suiteKeys = map[string]bool{"import_from": true, "plugins_dir": true, "modules": true, "client_guard": true, "alerts": true}

// SplitConfig separates suite keys from the upstream configuration. The
// returned YAML is the original document minus suite keys, with state_file
// defaulted to DefaultStateFile when absent.
func SplitConfig(raw []byte) (Config, []byte, error) {
	cfg := Config{PluginsDir: DefaultPluginsDir, Modules: map[string]string{}}
	var doc yaml.Node
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return Config{}, nil, fmt.Errorf("Parse plugin configuration: %w", err)
		}
	}
	var mapping *yaml.Node
	switch {
	case doc.Kind == 0:
		mapping = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping}}
	case doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode:
		mapping = doc.Content[0]
	default:
		// Let the upstream decoder produce its own error for odd documents.
		return cfg, raw, nil
	}
	kept := make([]*yaml.Node, 0, len(mapping.Content))
	hasStateFile := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if !suiteKeys[key.Value] {
			if key.Value == "state_file" {
				hasStateFile = true
				cfg.StateFile = strings.TrimSpace(value.Value)
			}
			kept = append(kept, key, value)
			continue
		}
		switch key.Value {
		case "import_from":
			if err := value.Decode(&cfg.ImportFrom); err != nil {
				return Config{}, nil, fmt.Errorf("Parse import_from: %w", err)
			}
			cfg.ImportFrom = strings.TrimSpace(cfg.ImportFrom)
		case "plugins_dir":
			if err := value.Decode(&cfg.PluginsDir); err != nil {
				return Config{}, nil, fmt.Errorf("Parse plugins_dir: %w", err)
			}
			if cfg.PluginsDir = strings.TrimSpace(cfg.PluginsDir); cfg.PluginsDir == "" {
				cfg.PluginsDir = DefaultPluginsDir
			}
		case "client_guard":
			if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
				break
			}
			if value.Kind != yaml.MappingNode {
				return Config{}, nil, fmt.Errorf("client_guard must be a mapping")
			}
			sub := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for j := 0; j+1 < len(value.Content); j += 2 {
				if value.Content[j].Value == "import_from" {
					if err := value.Content[j+1].Decode(&cfg.ClientGuardImportFrom); err != nil {
						return Config{}, nil, fmt.Errorf("Parse client_guard.import_from: %w", err)
					}
					cfg.ClientGuardImportFrom = strings.TrimSpace(cfg.ClientGuardImportFrom)
					continue
				}
				sub.Content = append(sub.Content, value.Content[j], value.Content[j+1])
			}
			encoded, err := yaml.Marshal(sub)
			if err != nil {
				return Config{}, nil, fmt.Errorf("Encode client_guard: %w", err)
			}
			cfg.ClientGuard = encoded
		case "alerts":
			if value.Kind != yaml.MappingNode && !(value.Kind == yaml.ScalarNode && value.Tag == "!!null") {
				return Config{}, nil, fmt.Errorf("alerts must be a mapping of setting to value")
			}
			if err := value.Decode(&cfg.Alerts); err != nil {
				return Config{}, nil, fmt.Errorf("Parse alerts: %w", err)
			}
		case "modules":
			if value.Kind != yaml.MappingNode && !(value.Kind == yaml.ScalarNode && value.Tag == "!!null") {
				return Config{}, nil, fmt.Errorf("modules must be a mapping of module ID to mode")
			}
			if err := value.Decode(&cfg.Modules); err != nil {
				return Config{}, nil, fmt.Errorf("Parse modules: %w", err)
			}
			if cfg.Modules == nil {
				cfg.Modules = map[string]string{}
			}
		}
	}
	if !hasStateFile || cfg.StateFile == "" {
		cfg.StateFile = DefaultStateFile
		if hasStateFile {
			// Replace the blank value in place.
			for i := 0; i+1 < len(kept); i += 2 {
				if kept[i].Value == "state_file" {
					kept[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: DefaultStateFile}
				}
			}
		} else {
			kept = append(kept,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "state_file"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: DefaultStateFile})
		}
	}
	mapping.Content = kept
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return Config{}, nil, fmt.Errorf("Encode plugin configuration: %w", err)
	}
	return cfg, out, nil
}
