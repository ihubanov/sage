package main

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `mcp-token create` mints a real, irreversible credential. `create --help` must
// print usage and return WITHOUT minting — the bug was that the parser's default
// branch silently swallowed --help and fell through to the mint (the outer
// dispatcher only handles --help at the subcommand position). Returning here
// before mcpTokenSigningIdentity/mcpTokenAPICall proves no token is issued: on
// the old code path --help reached the signing/API step and could not return nil.
func TestMCPTokenCreateHelpDoesNotMint(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Args = []string{"sage-gui", "mcp-token", "create", "--help"}
	err := runMCPTokenCreate()
	_ = w.Close()
	os.Stdout = oldStdout
	out, _ := io.ReadAll(r)

	require.NoError(t, err, "--help must print usage and return, never fall through to minting")
	assert.Contains(t, string(out), "sage-gui mcp-token create", "usage was printed")
}

// An unrecognized flag must ERROR rather than be silently ignored, because
// silently swallowing an unknown flag on a command with irreversible side
// effects is its own trap.
func TestMCPTokenCreateRejectsUnknownFlag(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"sage-gui", "mcp-token", "create", "--bogus"}
	err := runMCPTokenCreate()
	require.Error(t, err, "an unknown flag must not be swallowed on a command that mints a credential")
	assert.Contains(t, err.Error(), "unknown argument")
}
