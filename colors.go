package main

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ANSI SGR (Select Graphic Rendition) codes: the parameters for the
// "\x1b[<code>m" escape sequence that sets terminal text color/style,
// standardized by ECMA-48. These are exactly what `tput setaf N` (N=0-7)
// emits on every terminal in common use today (xterm, xterm-256color,
// screen, tmux, linux console, vt100) - the original bash prompt built its
// colors by calling `tput setaf 0` through `tput setaf 7` in a loop
// (~/.bashrc.d/all/bash_lib) and stashing the results in shell variables.
// Hardcoding them here just skips a `tput` subprocess call per lookup; it
// gives up the (essentially theoretical) portability of adapting to a
// terminfo entry that ever defined `setaf` differently, which none of the
// terminals above actually do.
const (
	sgrReset   = "0"
	sgrBlack   = "30"
	sgrRed     = "31"
	sgrGreen   = "32"
	sgrYellow  = "33"
	sgrBlue    = "34"
	sgrMagenta = "35"
	sgrCyan    = "36"
	sgrWhite   = "37"

	// "1;" is the bold/increased-intensity SGR code, combinable with a
	// color code in one escape sequence - used for the exit-status-colored
	// prompt symbol, matching the original's "$_bold$_red"/"$_bold$_green".
	sgrBoldRed   = "1;" + sgrRed
	sgrBoldGreen = "1;" + sgrGreen
)

// colorCodes maps the names usable in a promptbuilder-colors file to their
// SGR code.
var colorCodes = map[string]string{
	"black":   sgrBlack,
	"red":     sgrRed,
	"green":   sgrGreen,
	"yellow":  sgrYellow,
	"blue":    sgrBlue,
	"magenta": sgrMagenta,
	"cyan":    sgrCyan,
	"white":   sgrWhite,
	"default": sgrReset,
	"reset":   sgrReset,
}

// colorRule is one line from a promptbuilder-colors file: "user|host
// <glob-pattern> <color-name>".
type colorRule struct {
	kind    string // "user" or "host"
	pattern string
	sgr     string
}

// userConfigDir is where per-user promptbuilder config files live.
func userConfigDir() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "promptbuilder")
}

// systemConfigPath resolves filename against the system config directory:
// /etc/promptbuilder/<filename> if that file exists, else
// /usr/local/etc/promptbuilder/<filename>.
func systemConfigPath(filename string) string {
	etc := filepath.Join("/etc/promptbuilder", filename)
	if _, err := os.Stat(etc); err == nil {
		return etc
	}
	return filepath.Join("/usr/local/etc/promptbuilder", filename)
}

// configPaths returns the colors files, in precedence order (first match
// wins): the user's own file, then the system one. Both are directories
// rather than single dotfiles so other config (e.g. daemon timing
// overrides) can live alongside "colors" later without another path
// scheme.
func configPaths() []string {
	return []string{
		filepath.Join(userConfigDir(), "colors"),
		systemConfigPath("colors"),
	}
}

// parseColorRules reads one promptbuilder-colors file. A missing file is
// not an error - it just contributes no rules, matching the original
// bash setup where a machine or user with nothing configured just falls
// through to the built-in default table.
func parseColorRules(path string) []colorRule {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var rules []colorRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue // malformed - skip rather than fail the whole prompt
		}
		kind, pattern, colorName := fields[0], fields[1], strings.ToLower(fields[2])
		if kind != "user" && kind != "host" {
			continue
		}
		sgr, ok := colorCodes[colorName]
		if !ok {
			continue
		}
		rules = append(rules, colorRule{kind: kind, pattern: pattern, sgr: sgr})
	}
	return rules
}

var (
	loadedColorRules []colorRule
	colorRulesLoaded bool
)

// getColorRules loads both config files once per process (lazily, since
// not every invocation needs them) and caches the result - userColor and
// hostColor are each called more than once per render.
func getColorRules() []colorRule {
	if colorRulesLoaded {
		return loadedColorRules
	}
	colorRulesLoaded = true
	for _, p := range configPaths() {
		loadedColorRules = append(loadedColorRules, parseColorRules(p)...)
	}
	return loadedColorRules
}

// configuredColor returns the SGR code for the first rule of the given
// kind ("user" or "host") whose pattern matches name, checking the user's
// file before the machine's file (configPaths' order), so a user override
// always wins over the per-machine default.
func configuredColor(kind, name string) (string, bool) {
	for _, r := range getColorRules() {
		if r.kind != kind {
			continue
		}
		if ok, _ := path.Match(r.pattern, name); ok {
			return r.sgr, true
		}
	}
	return "", false
}
