package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clientauth"
)

// buildRemoteClient constructs a no-fallback API client for a resolved remote
// target. TLS options come from the target's context; the transport bearer comes
// from the context's credential_command (a cached clientauth.CredentialSource)
// or, for an ad-hoc --city-url/GC_CITY_URL target, from GC_CITY_URL_TOKEN. The
// resolver guarantees at most one credential technique per target.
func buildRemoteClient(target *remoteTarget) (*api.Client, error) {
	opts := api.RemoteOptions{}
	if ctx := target.Ctx; ctx != nil {
		opts.CAFile = ctx.CAFile
		opts.TLSServerName = ctx.TLSServerName
		opts.InsecureSkipVerify = ctx.InsecureSkipVerify
		if ctx.Timeout != "" {
			d, err := time.ParseDuration(ctx.Timeout)
			if err != nil {
				return nil, fmt.Errorf("context %q: invalid timeout %q: %w", ctx.Name, ctx.Timeout, err)
			}
			opts.RESTTimeout = d
		}
		if ctx.CredentialCommand != "" {
			cs, err := clientauth.NewCredentialSource(ctx.CredentialCommand, target.BaseURL, target.CityName, false)
			if err != nil {
				return nil, err
			}
			opts.Token = cs.Token
		}
	}
	if target.Token != "" {
		tok := target.Token
		opts.Token = func() (string, error) { return tok, nil }
	}
	return api.NewRemoteCityScopedClient(target.BaseURL, target.CityName, opts)
}

// resolveReadTarget resolves a no-argument READ command's target. For a REMOTE
// target (--context/--city-url/env/sticky default) it returns a no-fallback
// remote client with isRemote=true; the caller routes every read through it and,
// because a remote client is non-fallbackable (gate G1), a remote error is
// surfaced rather than fallen back. For a LOCAL target it returns isRemote=false
// and the resolved cityPath, and the caller uses its existing local client seam
// (preserving per-command test injection and the loopback fallback). A remote
// resolution or build failure is returned as err.
func resolveReadTarget() (remoteClient *api.Client, isRemote bool, cityPath string, err error) {
	ctx, err := resolveContextAllowRemote()
	if err != nil {
		return nil, false, "", err
	}
	if ctx.Remote != nil {
		c, berr := buildRemoteClient(ctx.Remote)
		if berr != nil {
			return nil, true, "", berr
		}
		return c, true, "", nil
	}
	return nil, false, ctx.CityPath, nil
}
