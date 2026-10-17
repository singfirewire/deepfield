//go:build !windows

package main

// Non-Windows hosts have no Win32 GUI here: fall back to the embedded web UI.
func runGUI(root string, workers int) {
	if root == "" {
		root = "."
	}
	cmdServe(root, 7788, true, workers)
}
