package web

import (
	"io/fs"
	"testing"
)

// The placeholder keeps dist present, so the package builds before `make web`.
func TestDist(t *testing.T) {
	if _, err := fs.Stat(Dist(), ".gitkeep"); err != nil {
		t.Fatal(err)
	}
}
