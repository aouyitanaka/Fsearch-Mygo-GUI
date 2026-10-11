//go:build !unix

package main

import "syscall"

func daemonAttr() *syscall.SysProcAttr { return nil }
