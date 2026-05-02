// Package hookinstall registers the sessionEnd hook entry into the user-level
// Copilot CLI configuration file (~/.copilot/config.json).
//
// Copilot CLI reads user-level hooks from the inline `hooks` field of
// config.json (see `copilot help config` -> "hooks"). The schema of each entry
// matches the per-repo `.github/hooks/*.json` files, but in config.json the
// definitions are inline (no outer `{ "version": 1, "hooks": ... }` wrapper).
package hookinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// installerMarker is an extra JSON field we set on entries we own, so that
// re-running --install replaces our previous entry in place even if the
// binary path changed (e.g. moved between $GOBIN values).
const installerMarker = "copilot-session-report"

type entry struct {
	Type        string `json:"type"`
	Bash        string `json:"bash,omitempty"`
	PowerShell  string `json:"powershell,omitempty"`
	TimeoutSec  int    `json:"timeoutSec,omitempty"`
	InstalledBy string `json:"installedBy,omitempty"`
}

// Install writes (or updates) the sessionEnd hook entry pointing at execPath
// inside configPath (typically ~/.copilot/config.json).
//
// All other top-level keys of config.json are preserved verbatim, although
// key ordering may change because Go's encoding/json emits map keys in
// alphabetical order.
//
// Re-running with a different execPath replaces the existing entry in place
// (matched via the installedBy marker) so the user never accumulates stale
// duplicates. When debug is true, --debug is appended to the hook commands
// so that future hook invocations also produce log files. Returns true if
// the file was changed.
func Install(configPath, execPath string, debug bool) (bool, error) {
	cfg, err := load(configPath)
	if err != nil {
		return false, err
	}

	bash := shellQuote(execPath)
	pwsh := "& " + powershellQuote(execPath)
	if debug {
		bash += " --debug"
		pwsh += " --debug"
	}

	newEntry := entry{
		Type:        "command",
		Bash:        bash,
		PowerShell:  pwsh,
		TimeoutSec:  300,
		InstalledBy: installerMarker,
	}

	encoded, err := json.Marshal(newEntry)
	if err != nil {
		return false, err
	}

	hooks := cfg.hooks()

	existing := hooks["sessionEnd"]
	updated := make([]json.RawMessage, 0, len(existing)+1)
	replaced := false
	for _, raw := range existing {
		var e entry
		if err := json.Unmarshal(raw, &e); err == nil && e.InstalledBy == installerMarker {
			if !replaced {
				updated = append(updated, encoded)
				replaced = true
			}

			continue
		}

		updated = append(updated, raw)
	}

	if !replaced {
		updated = append(updated, encoded)
	}

	hooks["sessionEnd"] = updated

	if err := cfg.setHooks(hooks); err != nil {
		return false, err
	}

	bakPath, err := backup(configPath)
	if err != nil {
		return false, err
	}

	if err := save(configPath, cfg); err != nil {
		return false, err
	}

	if bakPath != "" {
		if same, err := sameFile(bakPath, configPath); err == nil && same {
			_ = os.Remove(bakPath)
		}
	}

	return true, nil
}

// config wraps the raw top-level JSON object so we can preserve unknown
// fields when round-tripping config.json.
type config struct {
	raw map[string]json.RawMessage
}

func (c *config) hooks() map[string][]json.RawMessage {
	out := map[string][]json.RawMessage{}

	raw, ok := c.raw["hooks"]
	if !ok || len(raw) == 0 {
		return out
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		// Ignore malformed hooks block; we'll overwrite it.
		return map[string][]json.RawMessage{}
	}

	return out
}

func (c *config) setHooks(hooks map[string][]json.RawMessage) error {
	encoded, err := json.Marshal(hooks)
	if err != nil {
		return err
	}

	c.raw["hooks"] = encoded
	return nil
}

func sameFile(a, b string) (bool, error) {
	da, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}

	db, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}

	return bytes.Equal(da, db), nil
}

// shellQuote wraps s in POSIX-shell single quotes, doubling embedded
// single quotes via the standard `'\''` escape. The result is safe to
// drop into any sh/bash/zsh command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powershellQuote wraps s in PowerShell single quotes (literal, no
// expansion), doubling embedded single quotes per PowerShell rules.
func powershellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func load(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &config{raw: map[string]json.RawMessage{}}, nil
		}

		return nil, err
	}

	if len(data) == 0 {
		return &config{raw: map[string]json.RawMessage{}}, nil
	}

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return &config{raw: raw}, nil
}

// backup copies path to path+".bak". Returns the backup path on success,
// or empty string if the source file does not exist.
func backup(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", err
	}

	bak := path + ".bak"
	if err := os.WriteFile(bak, src, 0o600); err != nil {
		return "", err
	}

	return bak, nil
}

func save(path string, cfg *config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg.raw, "", "  ")
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return os.Rename(tmp, path)
}
