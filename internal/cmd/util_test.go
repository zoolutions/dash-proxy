package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The deploy tooling moved its secret names to DASH_PROXY_* with the rename
// (#122); containers booted by older tooling still carry KAMAL_PROXY_*. Both
// have to authenticate, and the current name has to win when both are set.
func TestTokenFromEnv_ReadsTheCurrentName(t *testing.T) {
	t.Setenv("DASH_PROXY_DOMAINS_TOKEN", "current")

	assert.Equal(t, "current", tokenFromEnv("DOMAINS_TOKEN"))
}

func TestTokenFromEnv_FallsBackToTheLegacyName(t *testing.T) {
	t.Setenv("DASH_PROXY_REFRESH_TOKEN", "")
	t.Setenv("KAMAL_PROXY_REFRESH_TOKEN", "legacy")

	assert.Equal(t, "legacy", tokenFromEnv("REFRESH_TOKEN"))
}

func TestTokenFromEnv_PrefersTheCurrentNameOverTheLegacyOne(t *testing.T) {
	t.Setenv("DASH_PROXY_REDIRECTS_TOKEN", "current")
	t.Setenv("KAMAL_PROXY_REDIRECTS_TOKEN", "legacy")

	assert.Equal(t, "current", tokenFromEnv("REDIRECTS_TOKEN"))
}

func TestTokenFromEnv_EmptyWhenNeitherIsSet(t *testing.T) {
	t.Setenv("DASH_PROXY_DOMAINS_TOKEN", "")
	t.Setenv("KAMAL_PROXY_DOMAINS_TOKEN", "")

	assert.Equal(t, "", tokenFromEnv("DOMAINS_TOKEN"))
}
