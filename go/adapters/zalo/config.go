// Package zalo implements F08 — the Zalo Official Account channel adapter for
// the gateway's channel registry.
//
// Ported from adapters/channels/zalo/src/channel_zalo (Python). The public
// surface mirrors the Go ChannelAdapter contract in internal/gateway/channels.
package zalo

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config carries the static per-OA Zalo configuration.
type Config struct {
	OAID                  string
	AppID                 string
	AppSecret             string
	OAAccessToken         string
	OARefreshToken        string
	APIBase               string
	OAuthBase             string
	TextChunkLimit        int
	RequestTimeoutSeconds float64
	TokenRefreshLeadS     float64
	TokenValiditySeconds  float64
	TokenFailureDisableS  float64
}

// Defaults exposed for callers and tests.
const (
	DefaultAPIBase                  = "https://openapi.zalo.me"
	DefaultOAuthBase                = "https://oauth.zaloapp.com"
	DefaultTextChunkLimit           = 2000
	DefaultRequestTimeoutSeconds    = 30.0
	DefaultTokenRefreshLeadSeconds  = 3600.0
	DefaultTokenValiditySeconds     = 86400.0
	DefaultTokenFailureDisableSecs  = 3600.0
)

// FromEnv builds a Config from environment variables.
//
// Required:
//
//	ZALO_OA_ID
//	ZALO_APP_ID
//	ZALO_APP_SECRET
//	ZALO_OA_ACCESS_TOKEN
//	ZALO_OA_REFRESH_TOKEN
//
// Optional:
//
//	ZALO_API_BASE                 (default: https://openapi.zalo.me)
//	ZALO_OAUTH_BASE               (default: https://oauth.zaloapp.com)
//	ZALO_TEXT_CHUNK_LIMIT         (default: 2000; must be in (0, 4000])
//	ZALO_REQUEST_TIMEOUT_S        (default: 30)
//	ZALO_TOKEN_REFRESH_LEAD_S     (default: 3600)
//	ZALO_TOKEN_VALIDITY_S         (default: 86400)
//	ZALO_TOKEN_FAILURE_DISABLE_S  (default: 3600)
func FromEnv(env map[string]string) (Config, error) {
	if env == nil {
		env = osEnvMap()
	}

	required := map[string]string{
		"ZALO_OA_ID":            "",
		"ZALO_APP_ID":           "",
		"ZALO_APP_SECRET":       "",
		"ZALO_OA_ACCESS_TOKEN":  "",
		"ZALO_OA_REFRESH_TOKEN": "",
	}
	for k := range required {
		v := strings.TrimSpace(env[k])
		if v == "" {
			return Config{}, fmt.Errorf("%s must be set", k)
		}
		required[k] = v
	}

	apiBase := strings.TrimRight(envOr(env, "ZALO_API_BASE", DefaultAPIBase), "/")
	oauthBase := strings.TrimRight(envOr(env, "ZALO_OAUTH_BASE", DefaultOAuthBase), "/")

	chunk, err := intEnv(env, "ZALO_TEXT_CHUNK_LIMIT", DefaultTextChunkLimit)
	if err != nil {
		return Config{}, err
	}
	if chunk <= 0 || chunk > 4000 {
		return Config{}, errors.New("ZALO_TEXT_CHUNK_LIMIT must be in (0, 4000]")
	}

	timeout, err := floatEnv(env, "ZALO_REQUEST_TIMEOUT_S", DefaultRequestTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	if timeout <= 0 {
		return Config{}, errors.New("ZALO_REQUEST_TIMEOUT_S must be > 0")
	}
	lead, err := floatEnv(env, "ZALO_TOKEN_REFRESH_LEAD_S", DefaultTokenRefreshLeadSeconds)
	if err != nil {
		return Config{}, err
	}
	if lead <= 0 {
		return Config{}, errors.New("ZALO_TOKEN_REFRESH_LEAD_S must be > 0")
	}
	validity, err := floatEnv(env, "ZALO_TOKEN_VALIDITY_S", DefaultTokenValiditySeconds)
	if err != nil {
		return Config{}, err
	}
	if validity <= 0 {
		return Config{}, errors.New("ZALO_TOKEN_VALIDITY_S must be > 0")
	}
	disableS, err := floatEnv(env, "ZALO_TOKEN_FAILURE_DISABLE_S", DefaultTokenFailureDisableSecs)
	if err != nil {
		return Config{}, err
	}
	if disableS <= 0 {
		return Config{}, errors.New("ZALO_TOKEN_FAILURE_DISABLE_S must be > 0")
	}

	return Config{
		OAID:                  required["ZALO_OA_ID"],
		AppID:                 required["ZALO_APP_ID"],
		AppSecret:             required["ZALO_APP_SECRET"],
		OAAccessToken:         required["ZALO_OA_ACCESS_TOKEN"],
		OARefreshToken:        required["ZALO_OA_REFRESH_TOKEN"],
		APIBase:               apiBase,
		OAuthBase:             oauthBase,
		TextChunkLimit:        chunk,
		RequestTimeoutSeconds: timeout,
		TokenRefreshLeadS:     lead,
		TokenValiditySeconds:  validity,
		TokenFailureDisableS:  disableS,
	}, nil
}

// withDefaults applies missing optional fields to keep callers terse.
func (c Config) withDefaults() Config {
	if c.APIBase == "" {
		c.APIBase = DefaultAPIBase
	}
	if c.OAuthBase == "" {
		c.OAuthBase = DefaultOAuthBase
	}
	if c.TextChunkLimit == 0 {
		c.TextChunkLimit = DefaultTextChunkLimit
	}
	if c.RequestTimeoutSeconds == 0 {
		c.RequestTimeoutSeconds = DefaultRequestTimeoutSeconds
	}
	if c.TokenRefreshLeadS == 0 {
		c.TokenRefreshLeadS = DefaultTokenRefreshLeadSeconds
	}
	if c.TokenValiditySeconds == 0 {
		c.TokenValiditySeconds = DefaultTokenValiditySeconds
	}
	if c.TokenFailureDisableS == 0 {
		c.TokenFailureDisableS = DefaultTokenFailureDisableSecs
	}
	return c
}

func envOr(env map[string]string, key, def string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return def
}

func intEnv(env map[string]string, key string, def int) (int, error) {
	raw, ok := env[key]
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q: %w", key, raw, err)
	}
	return v, nil
}

func floatEnv(env map[string]string, key string, def float64) (float64, error) {
	raw, ok := env[key]
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a float, got %q: %w", key, raw, err)
	}
	return v, nil
}

func osEnvMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}
