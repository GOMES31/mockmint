//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyReload delivers SIGHUP, which reloads packages.
func notifyReload(c chan<- os.Signal) { signal.Notify(c, syscall.SIGHUP) }
