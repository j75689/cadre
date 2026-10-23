package memory_test

import (
	"testing"

	"github.com/j75689/cadre/internal/storage"
	"github.com/j75689/cadre/internal/storage/conformance"
	"github.com/j75689/cadre/internal/storage/memory"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(*testing.T) conformance.Env {
		b := memory.NewBackend()
		return conformance.Env{
			Open:      func() storage.Driver { return b.Open() },
			Namespace: "team-a",
		}
	})
}
