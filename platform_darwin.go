//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// openWindow shows the UI in a chrome-less app window if Chrome, Edge or Brave
// is installed, otherwise in the default browser (Safari).
func openWindow() {
	home, _ := os.UserHomeDir()
	browsers := []string{
		"Google Chrome.app/Contents/MacOS/Google Chrome",
		"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"Brave Browser.app/Contents/MacOS/Brave Browser",
		"Chromium.app/Contents/MacOS/Chromium",
	}
	for _, base := range []string{"/Applications", filepath.Join(home, "Applications")} {
		for _, b := range browsers {
			p := filepath.Join(base, b)
			if _, err := os.Stat(p); err == nil {
				if exec.Command(p, "--app="+uiURL, "--window-size=1080,720").Start() == nil {
					return
				}
			}
		}
	}
	exec.Command("open", uiURL).Start()
}

func openPath(p string) { exec.Command("open", p).Start() }

func agentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.wildchat.agent.plist")
}

func startupEnabled() bool {
	_, err := os.Stat(agentPath())
	return err == nil
}

// setStartup adds or removes a LaunchAgent that starts WildChat in the background at login.
func setStartup(on bool) error {
	if !on {
		os.Remove(agentPath())
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.wildchat.agent</string>
  <key>ProgramArguments</key><array><string>%s</string><string>--background</string></array>
  <key>RunAtLoad</key><true/>
</dict></plist>
`, xmlEscape(exe))
	os.MkdirAll(filepath.Dir(agentPath()), 0o755)
	return os.WriteFile(agentPath(), []byte(plist), 0o644)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func showError(msg string) {
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, `\`, `\\`), `"`, `\"`)
	exec.Command("osascript", "-e", `display alert "WildChat" message "`+msg+`" as critical`).Run()
}

// isInstalled reports whether the app has been moved into /Applications.
func isInstalled() bool {
	exe, err := os.Executable()
	return err == nil && (strings.HasPrefix(exe, "/Applications/") || strings.Contains(exe, "/Applications/WildChat.app/"))
}

func cleanupLegacy() {}

func markHidden(string) {}
