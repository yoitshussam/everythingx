//go:build linux

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// revealLabel is the context-menu wording for handleOpenFile on this platform.
const revealLabel = "Open Containing Folder"

// openFile launches the given path with the system's default handler: a file
// opens in its associated application, a directory opens in the file manager.
func openFile(pathname string) {
	if pathname == "" {
		return
	}
	cmd := exec.Command("xdg-open", pathname)
	if err := cmd.Run(); err != nil {
		fmt.Println("Error:", err)
	}
}

// handleOpenFile opens the directory containing the given path using xdg-open,
// which launches the system's default file manager.
func handleOpenFile(pathname string) {
	if pathname == "" {
		return
	}
	dir := filepath.Dir(pathname)
	cmd := exec.Command("xdg-open", dir)
	if err := cmd.Run(); err != nil {
		fmt.Println("Error:", err)
	}
}
