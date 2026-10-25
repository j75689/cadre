package controller

import (
	"strings"
	"testing"
)

// A Runner's or an AgentAccess's name names and labels its token Secret:
// one that is not a DNS label is refused, and the message says what to do.
func TestInvalidName(t *testing.T) {
	for name, ok := range map[string]bool{
		"bot":                   true,
		"bot-access":            true,
		strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false,
		"pool.a":                false,
	} {
		msg := invalidName("Runner", name)
		if (msg == "") != ok {
			t.Errorf("%q: %q", name, msg)
		}
		if !ok && !strings.Contains(msg, "re-create this Runner under another name") {
			t.Errorf("%q: %q", name, msg)
		}
	}
}
