//go:build !windows

package main

// freshConsole: double-clicking a binary on Linux/macOS does not open a
// console window that would close on exit, so there is nothing to hold.
func freshConsole() bool { return false }
