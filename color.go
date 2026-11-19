package main

import (
	"os"
	"strings"
)

const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
	ansiGray  = "\x1b[90m"
)

var colorEnabled bool

// wantColor enables ANSI colors only for interactive terminals, so piped or
// redirected output (cron logs, files) stays plain. NO_COLOR and TERM=dumb
// are honored per convention.
func wantColor() bool {
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return isCharDevice(os.Stdout) && enableVT()
}

func colorize(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}
