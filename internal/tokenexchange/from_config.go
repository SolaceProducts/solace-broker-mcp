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

package tokenexchange

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
)

// FromConfig constructs an Exchanger from the validated broker_oauth
// config block and a pre-built HTTP client (production wires the
// retrying variant via idpclient.NewRetryingHTTPClient). This is the
// only file in the package that imports internal/config — tests use
// New(Params) directly.
//
// The config validator (internal/config.validateBrokerOAuthConfig) has
// already enforced structural validity at startup: non-empty fields,
// exactly-one client-auth sub-block, grant_type and audience_parameter_name
// in their respective allowlists. FromConfig translates the validated
// YAML-level types (strings, discriminated unions) into the typed Params
// enums the Exchanger expects.
func FromConfig(cfg *config.BrokerOAuthConfig, httpClient *http.Client, tokenCache cache.TokenCache) (*Exchanger, error) {
	if cfg == nil {
		return nil, fmt.Errorf("tokenexchange: broker_oauth config is nil")
	}

	authMethod, secret, err := resolveClientAuth(cfg.ClientAuth)
	if err != nil {
		return nil, err
	}

	grantType, err := resolveGrantType(cfg.GrantType)
	if err != nil {
		return nil, err
	}

	audienceParam, err := resolveAudienceParam(cfg.AudienceParam)
	if err != nil {
		return nil, err
	}

	// Resolve the breaker config: start from the shipped defaults and overlay
	// only the fields the operator set. A nil result means the operator
	// disabled the breaker (enabled: false) — Params.CircuitBreaker nil leaves
	// the breaker off while retries still run.
	breakerCfg := resolveCircuitBreakerConfig(cfg.CircuitBreaker)

	return New(Params{
		TokenURL:         cfg.TokenURL,
		ClientID:         cfg.ClientID,
		ClientAuthMethod: authMethod,
		ClientSecret:     secret,
		GrantType:        grantType,
		AudienceParam:    audienceParam,
		HTTPClient:       httpClient,
		Cache:            tokenCache,
		CircuitBreaker:   breakerCfg,
	})
}

// resolveCircuitBreakerConfig overlays operator-set fields onto the shipped
// defaults. Returns nil when the operator disabled the breaker (enabled:
// false), which New reads as "breaker off". An omitted block or omitted field
// takes the default.
func resolveCircuitBreakerConfig(cb *config.BrokerCircuitBreakerConfig) *CircuitBreakerConfig {
	if cb != nil && cb.Enabled != nil && !*cb.Enabled {
		slog.Warn("token exchange circuit breaker is DISABLED by configuration; " +
			"the IdP is unprotected against failure storms (not recommended in production)")
		return nil
	}

	resolved := DefaultCircuitBreakerConfig()
	if cb != nil {
		if cb.FailureRateWindow != nil {
			resolved.FailureRateWindow = *cb.FailureRateWindow
		}
		if cb.MinimumRequests != nil {
			resolved.MinimumRequests = *cb.MinimumRequests
		}
		if cb.FailureRateThresholdPercent != nil {
			resolved.FailureRateThresholdPercent = *cb.FailureRateThresholdPercent
		}
		if cb.ConsecutiveFailureThreshold != nil {
			resolved.ConsecutiveFailureThreshold = *cb.ConsecutiveFailureThreshold
		}
		if cb.OpenStateDuration != nil {
			resolved.OpenStateDuration = *cb.OpenStateDuration
		}
		if cb.HalfOpenProbeRequests != nil {
			resolved.HalfOpenProbeRequests = *cb.HalfOpenProbeRequests
		}
	}
	return &resolved
}

// resolveClientAuth determines the client auth method and extracts the
// secret from the populated sub-block. The config validator already
// enforced exactly-one-sub-block at startup, so the both-nil / both-set
// cases are programming errors, not operator errors.
func resolveClientAuth(auth config.BrokerClientAuth) (ClientAuthMethod, string, error) {
	switch {
	case auth.ClientSecretBasic != nil && auth.ClientSecretPost != nil:
		return 0, "", fmt.Errorf("tokenexchange: both client_secret_basic and client_secret_post are populated")
	case auth.ClientSecretBasic != nil:
		return ClientSecretBasic, auth.ClientSecretBasic.Secret, nil
	case auth.ClientSecretPost != nil:
		return ClientSecretPost, auth.ClientSecretPost.Secret, nil
	default:
		return 0, "", fmt.Errorf("tokenexchange: no client auth method configured")
	}
}

func resolveGrantType(gt string) (GrantType, error) {
	switch gt {
	case config.GrantTypeTokenExchange:
		return GrantTypeTokenExchange, nil
	default:
		return 0, fmt.Errorf("tokenexchange: unsupported grant_type %q", gt)
	}
}

func resolveAudienceParam(ap string) (AudienceFormat, error) {
	switch ap {
	case config.AudienceParamAudience:
		return AudienceParamAudience, nil
	case config.AudienceParamScope:
		return 0, fmt.Errorf("tokenexchange: audience_parameter_name %q is schema-accepted but not yet implemented", ap)
	case config.AudienceParamResource:
		return 0, fmt.Errorf("tokenexchange: audience_parameter_name %q is schema-accepted but not yet implemented", ap)
	default:
		return 0, fmt.Errorf("tokenexchange: unsupported audience_parameter_name %q", ap)
	}
}
