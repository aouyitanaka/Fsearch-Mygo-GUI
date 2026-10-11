//go:build unix

package main

import "syscall"

// daemonAttr starts the daemon in its own session, so closing the app does
// not take it down.
func daemonAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
