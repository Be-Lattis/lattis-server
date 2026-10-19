//go:build !linux

package webapp

import "os/exec"

// isolate : rien hors de Linux (LATTIS_WEB_ENGINE_UIDS y est refusé).
func isolate(*exec.Cmd, int) {}

func ownTree(string, int) error { return nil }
