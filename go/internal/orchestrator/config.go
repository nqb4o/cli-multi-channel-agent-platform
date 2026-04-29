// Environment-driven configuration for the orchestrator service.
//
// All knobs are read from the environment at startup. The orchestrator
// is the ONLY service that talks to Daytona, so credential configuration
// lives here.
//
// Env vars:
//
//	DAYTONA_API_KEY        Required for live mode. Empty -> in-memory fake
//	                       (used by unit tests and local dev without Daytona).
//	DAYTONA_API_URL        Optional override (defaults to Daytona public API).
//	DAYTONA_TARGET         Optional region/target.
//	SANDBOX_IMAGE          Container image to launch. Defaults to
//	                       "ubuntu:24.04" so spike work can run before the
//	                       image is published.
//	ORCHESTRATOR_HOST      HTTP bind host. Default 0.0.0.0.
//	ORCHESTRATOR_PORT      HTTP bind port. Default 8081.
//	AUTO_STOP_INTERVAL_M   Idle minutes before Daytona auto-hibernates the
//	                       sandbox. Default 5.
package orchestrator

import (
	"os"
	"strconv"
)

// Config holds the resolved orchestrator config.
type Config struct {
	DaytonaAPIKey     string
	DaytonaAPIURL     string
	DaytonaTarget     string
	SandboxImage      string
	Host              string
	Port              int
	AutoStopIntervalM int
}

// UseFakeDaytona reports whether no Daytona credentials are configured
// (in which case the in-memory fake should be wired).
func (c Config) UseFakeDaytona() bool {
	return c.DaytonaAPIKey == ""
}

// LoadConfig loads config from environment. Pure — safe to call at
// import time.
func LoadConfig() Config {
	port := 8081
	if v := os.Getenv("ORCHESTRATOR_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	autoStop := 5
	if v := os.Getenv("AUTO_STOP_INTERVAL_M"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			autoStop = n
		}
	}
	host := os.Getenv("ORCHESTRATOR_HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	image := os.Getenv("SANDBOX_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	return Config{
		DaytonaAPIKey:     os.Getenv("DAYTONA_API_KEY"),
		DaytonaAPIURL:     os.Getenv("DAYTONA_API_URL"),
		DaytonaTarget:     os.Getenv("DAYTONA_TARGET"),
		SandboxImage:      image,
		Host:              host,
		Port:              port,
		AutoStopIntervalM: autoStop,
	}
}
