package orchestrator

import (
	"os"
	"strings"
)

// statFile is a small helper used by tests to assert file presence without
// importing os in every test file.
func statFile(p string) error {
	_, err := os.Stat(p)
	return err
}

// containsLine returns true if any element of lines contains substr.
func containsLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
