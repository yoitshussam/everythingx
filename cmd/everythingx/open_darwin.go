//go:build darwin

package main

import (
	"fmt"
	"os/exec"
)

// revealLabel is the context-menu wording for handleOpenFile on this platform.
const revealLabel = "Reveal in Finder"

// openFile launches the given path with the system's default handler: a file
// opens in its associated application, a directory opens in the file manager.
func openFile(pathname string) {
	if pathname == "" {
		return
	}
	cmd := exec.Command("open", pathname)
	if err := cmd.Run(); err != nil {
		fmt.Println("Error:", err)
	}
}

// handleOpenFile reveals the given path in Finder using "open -R".
func handleOpenFile(pathname string) {
	if pathname == "" {
		return
	}
	cmd := exec.Command("open", "-R", pathname)
	if err := cmd.Run(); err != nil {
		fmt.Println("Error:", err)
	}
}
