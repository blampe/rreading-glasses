package internal

import (
	"os"
	"testing"

	charm "github.com/charmbracelet/log"
)

func TestMain(m *testing.M) {
	SetLogLevel(charm.DebugLevel)

	os.Exit(m.Run())
}
