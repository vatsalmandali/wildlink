//go:build !windows && !darwin

package main

import (
	"errors"
	"log"
	"os/exec"
	"runtime"
)

func opener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func openWindow()          { exec.Command(opener(), uiURL).Start() }
func openPath(p string)    { exec.Command(opener(), p).Start() }
func startupEnabled() bool { return false }
func setStartup(bool) error {
	return errors.New("start with system is only supported on Windows")
}

func showError(msg string) { log.Print(msg) }
func isInstalled() bool    { return false }
func cleanupLegacy()       {}

func markHidden(string) {}
