package mcp

import (
	"log/slog"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// FailForTest exposes the tool-error mapping to the external tests.
func FailForTest(log *slog.Logger, err error) *sdk.CallToolResult {
	return (&Server{log: log}).fail(err)
}

// SetDiscoveryTimeoutForTest shortens the discovery bound and returns a
// function restoring it.
func SetDiscoveryTimeoutForTest(d time.Duration) func() {
	old := discoveryTimeout
	discoveryTimeout = d
	return func() { discoveryTimeout = old }
}

// ObjectSchemaForTest exposes the input schema check.
func ObjectSchemaForTest(schema any) error {
	_, err := objectSchema(schema)
	return err
}
