//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var getConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// freshConsole reports whether this process is the only one attached to its
// console. Windows creates such a console when the exe is double-clicked
// (or started from a shortcut); that window disappears as soon as we exit.
// When launched from an existing terminal the shell shares the console.
func freshConsole() bool {
	var pids [2]uint32
	n, _, _ := getConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	return n == 1
}
