// Copyright 2015 Light Code Labs, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpserver

import (
	"fmt"
	"net"
	"net/http"

	"github.com/mholt/caddy"
	"github.com/mholt/caddy/caddytls"
)

func activateHTTPS(cctx caddy.Context) error {
	operatorPresent := !caddy.Started()

	if !caddy.Quiet && operatorPresent {
		fmt.Print("Activating privacy features... ")
	}

	ctx := cctx.(*httpContext)

	// pre-screen each config and earmark the ones that qualify for managed TLS
	markQualifiedForAutoHTTPS(ctx.siteConfigs)

	// place certificates and keys on disk
	for _, c := range ctx.siteConfigs {
		if c.TLS.OnDemand {
			continue // obtain these certificates on-demand instead
		}
		err := c.TLS.ObtainCert(c.TLS.Hostname, operatorPresent)
		if err != nil {
			return err
		}
	}

	// update TLS configurations
	err := enableAutoHTTPS(ctx.siteConfigs, true)
	if err != nil {
		return err
	}

	// set up redirects
	ctx.siteConfigs = makePlaintextRedirects(ctx.siteConfigs)

	// renew all relevant certificates that need renewal. this is important
	// to do right away so we guarantee that renewals aren't missed, and
	// also the user can respond to any potential errors that occur.
	// (skip if upgrading, because the parent process is likely already listening
	// on the ports we'd need to do ACME before we finish starting; parent process
	// already running renewal ticker, so renewal won't be missed anyway.)
	if !caddy.IsUpgrade() {
		err = caddytls.RenewManagedCertificates(true)
		if err != nil {
			return err
		}
	}

	if !caddy.Quiet && operatorPresent {
		fmt.Println("done.")
	}

	return nil
}

// markQualifiedForAutoHTTPS scans each config and, if it
// qualifies for managed TLS, it sets the Managed field of
// the TLS config to true.
func markQualifiedForAutoHTTPS(configs []*SiteConfig) {
	for _, cfg := range configs {
		if caddytls.QualifiesForManagedTLS(cfg) && cfg.Addr.Scheme != "http" {
			cfg.TLS.Managed = true
		}
	}
}

// enableAutoHTTPS configures each config to use TLS according to default settings.
// It will only change configs that are marked as managed but not on-demand, and
// assumes that certificates and keys are already on disk. If loadCertificates is
// true, the certificates will be loaded from disk into the cache for this process
// to use. If false, TLS will still be enabled and configured with default settings,
// but no certificates will be parsed loaded into the cache, and the returned error
// value will always be nil.
func enableAutoHTTPS(configs []*SiteConfig, loadCertificates bool) error {
	for _, cfg := range configs {
		if cfg == nil || cfg.TLS == nil || !cfg.TLS.Managed || cfg.TLS.OnDemand {
			continue
		}
		cfg.TLS.Enabled = true
		cfg.Addr.Scheme = "https"
		if loadCertificates && caddytls.HostQualifies(cfg.Addr.Host) {
			_, err := cfg.TLS.CacheManagedCertificate(cfg.Addr.Host)
			if err != nil {
				return err
			}
		}

		// Make sure any config values not explicitly set are set to default
		caddytls.SetDefaultTLSParams(cfg.TLS)

		// Set default port of 443 if not explicitly set
		if cfg.Addr.Port == "" &&
			cfg.TLS.Enabled &&
			(!cfg.TLS.Manual || cfg.TLS.OnDemand) &&
			cfg.Addr.Host != "localhost" {
			cfg.Addr.Port = HTTPSPort
		}
	}
	return nil
}

// makePlaintextRedirects sets up redirects from port 80 to the relevant HTTPS
// hosts. You must pass in all configs, not just configs that qualify, since
// we must know whether the same host already exists on port 80, and those would
// not be in a list of configs that qualify for automatic HTTPS. This function will
// only set up redirects for configs that qualify. It returns the updated list of
// all configs.
func makePlaintextRedirects(allConfigs []*SiteConfig) []*SiteConfig {
	for i, cfg := range allConfigs {
		if cfg.TLS.Managed &&
			!hostHasOtherPort(allConfigs, i, HTTPPort) &&
			(cfg.Addr.Port == HTTPSPort || !hostHasOtherPort(allConfigs, i, HTTPSPort)) {
			allConfigs = append(allConfigs, redirPlaintextHost(cfg))
		}
	}
	return allConfigs
}

// hostHasOtherPort returns true if there is another config in the list with the same
// hostname that has port otherPort, or false otherwise. All the configs are checked
// against the hostname of allConfigs[thisConfigIdx].
func hostHasOtherPort(allConfigs []*SiteConfig, thisConfigIdx int, otherPort string) bool {
	for i, otherCfg := range allConfigs {
		if i == thisConfigIdx {
			continue // has to be a config OTHER than the one we're comparing against
		}
		if otherCfg.Addr.Host == allConfigs[thisConfigIdx].Addr.Host &&
			otherCfg.Addr.Port == otherPort {
			return true
		}
	}
	return false
}

// redirPlaintextHost returns a new plaintext HTTP configuration for
// a virtualHost that simply redirects to cfg, which is assumed to
// be the HTTPS configuration. The returned configuration is set
// to listen on HTTPPort. The TLS field of cfg must not be nil.
func redirPlaintextHost(cfg *SiteConfig) *SiteConfig {
	redirMiddleware := func(next Handler) Handler {
		return HandlerFunc(func(w http.ResponseWriter, r *http.Request) (int, error) {
			// Construct the URL to which to redirect. Note that the Host in a
			// request might contain a port, but we just need the hostname.
			toURL := "https://"
			requestHost, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				requestHost = r.Host // host did not contain a port; okay
			}

			// The rest of the URL will consist of the hostname and the URI.
			// We do not append a port because if the HTTPSPort is changed
			// from the default value, it is probably because there is port
			// forwarding going on; and we do not need to specify the default
			// HTTPS port in the redirect. Serving HTTPS on a port other than
			// 443 is unusual, and is considered an advanced use case. If port
			// forwarding IS happening, then redirecting the external client to
			// this internal port will cause the connection to fail; and it
			// definitely causes ACME HTTP-01 challenges to fail, because it
			// only allows redirecting to port 80 or 443 (as of Feb 2018).
			// If a user wants to redirect HTTP to HTTPS on an external port
			// other than 443, they can easily configure that themselves.
			toURL += requestHost
			toURL += r.URL.RequestURI()

			w.Header().Set("Connection", "close")
			http.Redirect(w, r, toURL, http.StatusMovedPermanently)
			return 0, nil
		})
	}

	host := cfg.Addr.Host
	port := HTTPPort
	addr := net.JoinHostPort(host, port)

	return &SiteConfig{
		Addr:       Address{Original: addr, Host: host, Port: port},
		ListenHost: cfg.ListenHost,
		middleware: []Middleware{redirMiddleware},
		TLS:        &caddytls.Config{AltHTTPPort: cfg.TLS.AltHTTPPort, AltTLSSNIPort: cfg.TLS.AltTLSSNIPort},
		Timeouts:   cfg.Timeouts,
	}
}
