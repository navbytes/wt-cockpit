// Package config loads wtd's optional TOML config file
// (~/.config/wtcockpit/config.toml by default). It only parses and validates —
// it knows nothing about flags. Precedence (explicit flags > config > built-in
// defaults) is resolved by the caller (cmd/wtd), which knows which flags the
// user actually passed.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
)

// RepoConfig is a per-repo override, keyed by the repo's absolute path in
// Config.Repos: `[repos."/abs/path"] base = "develop"`.
type RepoConfig struct {
	Base string `toml:"base"`
}

// Config is the on-disk shape of config.toml. The zero value (returned when
// the file doesn't exist) means "nothing configured" — every field left unset.
type Config struct {
	Roots    []string              `toml:"roots"`
	Base     string                `toml:"base"`
	Socket   string                `toml:"socket"`
	TCP      string                `toml:"tcp"`
	State    string                `toml:"state"`
	Interval time.Duration         `toml:"interval"`
	Watch    string                `toml:"watch"`
	Repos    map[string]RepoConfig `toml:"repos"`
	Rules    []guardrail.Rule      `toml:"rules"`

	// RulesSet is true iff [[rules]] appeared in the file at all (even empty).
	// An absent [[rules]] means "use guardrail.DefaultRules()"; a present one —
	// however short — replaces the defaults outright, because append-only
	// would make the defaults un-disableable.
	RulesSet bool `toml:"-"`
}

// RulesOr returns the config's own rules if [[rules]] was present in the file,
// or defaults otherwise.
func (c Config) RulesOr(defaults []guardrail.Rule) []guardrail.Rule {
	if c.RulesSet {
		return c.Rules
	}
	return defaults
}

// DefaultPath returns the conventional config location: $XDG_CONFIG_HOME/
// wtcockpit/config.toml, or ~/.config/wtcockpit/config.toml if unset.
func DefaultPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "wtcockpit", "config.toml")
}

// Load reads and parses the config file at path. A missing file is not an
// error: it returns the zero Config, so callers fall back to built-in
// defaults. Malformed TOML, or a key this version doesn't recognise (a typo in
// a hand-edited guardrails file is a real footgun otherwise), is an error.
func Load(path string) (Config, error) {
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("%s: unknown config key %q", path, undecoded[0])
	}
	cfg.RulesSet = meta.IsDefined("rules")
	return cfg, nil
}
