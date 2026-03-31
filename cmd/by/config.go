package main

import (
	"github.com/cynkra/blockyard/internal/apiclient"
	"github.com/cynkra/blockyard/internal/cliconfig"
)

// mustClient creates a client from resolved credentials or exits with an error.
func mustClient(jsonOutput bool) *apiclient.Client {
	serverURL, token, err := cliconfig.ResolveCredentials()
	if err != nil {
		exitError(jsonOutput, err)
	}
	c := apiclient.New(serverURL, token)
	c.Version = version
	return c
}

// mustStreamingClient creates a streaming client from resolved credentials.
func mustStreamingClient(jsonOutput bool) *apiclient.Client {
	serverURL, token, err := cliconfig.ResolveCredentials()
	if err != nil {
		exitError(jsonOutput, err)
	}
	c := apiclient.NewStreaming(serverURL, token)
	c.Version = version
	return c
}
