// The private pane shell bypasses user profiles that can shadow proxy shims.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	engine := os.Getenv("HERDR_PROXY_SHELL_ENGINE")
	proxyPath := filepath.SplitList(os.Getenv("HERDR_PROXY_PATH"))
	if os.Getenv("HERDR_PROXY_MODE") != "1" || !filepath.IsAbs(engine) || len(proxyPath) == 0 || !filepath.IsAbs(proxyPath[0]) || len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "Proxy pane shell refused unsafe environment.")
		os.Exit(1)
	}
	// Packaged PowerShell can rebuild PATH at startup. Restore the isolated
	// inherited path after startup, before any interactive command can run.
	child := exec.Command(engine, "-NoLogo", "-NoProfile", "-NoExit", "-Command", "$env:PATH = $env:HERDR_PROXY_PATH")
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "Proxy pane shell could not start.")
		os.Exit(1)
	}
}
