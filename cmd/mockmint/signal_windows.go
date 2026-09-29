package main

import "os"

// notifyReload is a no-op: Windows has no SIGHUP; use POST /admin/reload.
func notifyReload(chan<- os.Signal) {}
