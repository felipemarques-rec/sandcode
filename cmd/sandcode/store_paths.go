package main

import (
	"os"
	"path/filepath"
)

// resolveStorePath returns the SQLite path used by sandcode in cwd. We anchor
// it under .sandcode/store.db so each project has an independent history.
func resolveStorePath(cwd string) string {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ""
		}
	}
	return filepath.Join(cwd, ".sandcode", "store.db")
}
