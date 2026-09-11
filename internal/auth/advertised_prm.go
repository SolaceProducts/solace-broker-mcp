// Copyright 2024-2026 Solace Corporation. All rights reserved.
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

package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const prmBarePath = "/.well-known/oauth-protected-resource"

// AdvertisedPRM is the single configured view of RFC 9728 PRM.
// It owns the resource_metadata URL in WWW-Authenticate, the
// oauth-protected-resource paths, served metadata, and startup log.
type AdvertisedPRM struct {
	handler             http.Handler
	metadata            *oauthex.ProtectedResourceMetadata
	resourceMetadataURL string
	paths               []string
}

// AdvertisedPRMInput names the three fields NewAdvertisedPRM needs.
// Pass by value; callers own the source config and copy these fields at the call site.
type AdvertisedPRMInput struct {
	Mode        string // one of config.AuthMode* constants
	ResourceURL string
	Issuer      string
}

// NewAdvertisedPRM builds the RFC 9728 PRM snapshot from in.
// Always returns non-nil; disabled and static return an empty snapshot.
func NewAdvertisedPRM(in AdvertisedPRMInput) *AdvertisedPRM {
	prm := &AdvertisedPRM{}
	if in.Mode == config.AuthModeDisabled {
		return prm
	}

	if in.ResourceURL != "" {
		parsedURL, _ := url.Parse(in.ResourceURL)
		prm.resourceMetadataURL = fmt.Sprintf("%s://%s%s", parsedURL.Scheme, parsedURL.Host, prmBarePath)
	}
	if in.Mode != config.AuthModeOAuth {
		return prm
	}

	prm.metadata = &oauthex.ProtectedResourceMetadata{
		Resource:               in.ResourceURL,
		AuthorizationServers:   []string{in.Issuer},
		ScopesSupported:        []string{"openid"},
		BearerMethodsSupported: []string{"header"},
	}
	prm.handler = sdkauth.ProtectedResourceMetadataHandler(prm.metadata)
	prm.paths = []string{prmBarePath}
	parsedURL, _ := url.Parse(in.ResourceURL)
	if resourcePath := strings.TrimRight(parsedURL.Path, "/"); resourcePath != "" {
		prm.paths = append(prm.paths, prmBarePath+resourcePath)
	}
	return prm
}

// Handler returns the configured PRM handler, or nil outside OAuth mode.
func (p *AdvertisedPRM) Handler() http.Handler {
	return p.handler
}

// ResourceMetadataURL returns the URL advertised on a 401 response.
func (p *AdvertisedPRM) ResourceMetadataURL() string {
	return p.resourceMetadataURL
}

// Paths returns the local paths that serve PRM.
func (p *AdvertisedPRM) Paths() []string {
	return p.paths
}

// Log emits the OAuth PRM registration snapshot when routes exist.
func (p *AdvertisedPRM) Log() {
	if p.metadata == nil || len(p.paths) == 0 {
		return
	}

	issuers := make([]string, len(p.metadata.AuthorizationServers))
	for i, issuer := range p.metadata.AuthorizationServers {
		issuers[i] = config.SanitizeURLString(issuer)
	}
	slog.Info("registered OAuth protected resource metadata endpoint",
		slog.String("resource", config.SanitizeURLString(p.metadata.Resource)),
		slog.Any("issuers", issuers),
		slog.Any("scopes_supported", p.metadata.ScopesSupported),
		slog.Any("bearer_methods_supported", p.metadata.BearerMethodsSupported),
		slog.String("resource_metadata_url", config.SanitizeURLString(p.resourceMetadataURL)),
		slog.Any("prm_paths", p.paths),
	)
}
