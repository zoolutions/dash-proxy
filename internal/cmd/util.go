package cmd

import (
	"cmp"
	"fmt"
	"net/rpc"
	"os"
	"strconv"
	"time"
)

const (
	ENV_PREFIX = "KAMAL_PROXY_"

	// The name the proxy has carried since the rename (#122). ENV_PREFIX stays
	// for the flag-override mechanism and for deploy tooling that predates it.
	CURRENT_ENV_PREFIX = "DASH_PROXY_"
)

// tokenFromEnv reads a bearer token the dynamic-source pollers and the refresh
// nudge authenticate with. DASH_PROXY_<name> wins; KAMAL_PROXY_<name> is the
// fallback, the same way Config.SocketPath treats the socket override, so a
// deploy tool that still sets the old name keeps working while one that has
// moved to the new name gets it honoured.
func tokenFromEnv(name string) string {
	return cmp.Or(os.Getenv(CURRENT_ENV_PREFIX+name), os.Getenv(ENV_PREFIX+name))
}

// ensureDataDir creates the --data-dir when one was supplied, so state and
// certificate files can be written into it on first use. Shared by every
// command that writes into the data directory (`run`, `import certs`).
func ensureDataDir() error {
	if globalConfig.AlternateConfigDir == "" {
		return nil
	}

	if err := os.MkdirAll(globalConfig.AlternateConfigDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory %q: %w", globalConfig.AlternateConfigDir, err)
	}
	return nil
}

func withRPCClient(socketPath string, fn func(client *rpc.Client) error) error {
	client, err := rpc.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer client.Close()
	return fn(client)
}

func findEnv(key string) (string, bool) {
	value, ok := os.LookupEnv(ENV_PREFIX + key)
	if ok {
		return value, true
	}

	value, ok = os.LookupEnv(key)
	if ok {
		return value, true
	}

	return "", false
}

func getEnvInt(key string, defaultValue int) int {
	value, ok := findEnv(key)
	if !ok {
		return defaultValue
	}

	intValue, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}

	return intValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value, ok := findEnv(key)
	if !ok {
		return defaultValue
	}

	durationValue, err := time.ParseDuration(value)
	if err != nil {
		return defaultValue
	}

	return durationValue
}

func getEnvBool(key string, defaultValue bool) bool {
	value, ok := findEnv(key)
	if !ok {
		return defaultValue
	}

	boolValue, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}

	return boolValue
}

func getEnvString(key string, defaultValue string) string {
	value, ok := findEnv(key)
	if !ok {
		return defaultValue
	}
	return value
}
