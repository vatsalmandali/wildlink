//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func hidden(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}

// openWindow shows the UI in a chrome-less Edge app window, falling back to the default browser.
func openWindow() {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			if exec.Command(c, "--app="+uiURL, "--window-size=1080,720").Start() == nil {
				return
			}
		}
	}
	hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", uiURL)).Start()
}

func openPath(p string) { exec.Command("explorer", p).Start() }

func startupEnabled() bool {
	return hidden(exec.Command("reg", "query", runKey, "/v", appName)).Run() == nil
}

func setStartup(on bool) error {
	if !on {
		hidden(exec.Command("reg", "delete", runKey, "/v", appName, "/f")).Run()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return hidden(exec.Command("reg", "add", runKey, "/v", appName, "/t", "REG_SZ",
		"/d", `"`+exe+`" --background`, "/f")).Run()
}

func showError(msg string) {
	t, _ := syscall.UTF16PtrFromString("WildChat")
	m, _ := syscall.UTF16PtrFromString(msg)
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}

// isInstalled reports whether we're running from the installer's location.
func isInstalled() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	pf := strings.ToLower(os.Getenv("ProgramFiles"))
	return pf != "" && strings.HasPrefix(strings.ToLower(exe), pf)
}

// cleanupLegacy removes auto-start entries left by the earlier LAN Chat and WildChat builds.
func cleanupLegacy() {
	for _, old := range []string{"LANChat", "WildChat"} {
		hidden(exec.Command("reg", "delete", runKey, "/v", old, "/f")).Run()
	}
}

// markHidden keeps part-finished transfers out of sight in Explorer.
func markHidden(path string) {
	if p, err := syscall.UTF16PtrFromString(path); err == nil {
		syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_HIDDEN)
	}
}
