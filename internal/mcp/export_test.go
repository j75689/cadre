package mcp

import (
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// FailForTest exposes the tool-error mapping to the external tests.
func FailForTest(log *slog.Logger, err error) *sdk.CallToolResult {
	return (&Server{log: log}).fail(err)
}
