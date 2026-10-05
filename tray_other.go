//go:build !windows

package main

import (
	"os/exec"
	"runtime"
	"strings"
)

func startTray(a *App) {}

// notifyOS shows a native notification (Notification Centre on macOS).
func notifyOS(title, body string) {
	if title == "" {
		return
	}
	esc := func(s string) string {
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	}
	if runtime.GOOS == "darwin" {
		script := `display notification "` + esc(body) + `" with title "` + esc(appName) + `" subtitle "` + esc(title) + `" sound name "Ping"`
		exec.Command("osascript", "-e", script).Run()
		return
	}
	exec.Command("notify-send", "-a", appName, title, body).Run()
}
