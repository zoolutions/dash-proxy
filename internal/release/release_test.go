// Package release holds the tests for bin/release, the release driver.
//
// The script is shell, but its version arithmetic and its backfill planning
// are the parts that can silently ship a wrong tag, so they are covered here
// and run as part of `make test`. Every case drives the real script against a
// throwaway git repository; `gh` is stubbed on PATH so nothing touches the
// network.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseScript resolves bin/release relative to this package.
func releaseScript(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	path := filepath.Join(wd, "..", "..", "bin", "release")
	_, err = os.Stat(path)
	require.NoError(t, err, "bin/release must exist")

	return path
}

// testEnv strips the developer's own git configuration, whose hooks and
// templates otherwise write into the throwaway repository while it is being
// torn down.
func testEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

// testRepo builds a throwaway git repository with the given tags, each on its
// own commit, and bin/release installed at the path it lives at for real --
// the script resolves the repository from its own location, so a copy is what
// gives each case its own tag history.
func testRepo(t *testing.T, tags ...string) string {
	t.Helper()

	dir := t.TempDir()

	script, err := os.ReadFile(releaseScript(t))
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "release"), script, 0o755))

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(testEnv(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	run("init", "-q", "-b", "main")
	// git's background auto-maintenance outlives the test and races t.TempDir's
	// cleanup on the object store.
	run("config", "gc.auto", "0")
	run("commit", "-q", "--allow-empty", "-m", "initial commit")
	for _, tag := range tags {
		run("commit", "-q", "--allow-empty", "-m", "work for "+tag)
		run("tag", "-a", tag, "-m", tag)
	}

	return dir
}

// stubGH puts a fake `gh` on PATH that reports the given tags as already
// having a GitHub release, so no case reaches the network.
func stubGH(t *testing.T, releasedTags ...string) string {
	t.Helper()

	dir := t.TempDir()
	script := "#!/bin/bash\n" +
		"if [ \"$1\" = \"release\" ] && [ \"$2\" = \"list\" ]; then\n" +
		"cat <<'EOT'\n" +
		strings.Join(releasedTags, "\n") + "\nEOT\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"gh $*\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755))

	return dir
}

// runRelease invokes bin/release inside repo with `gh` stubbed on PATH.
func runRelease(t *testing.T, repo, ghDir string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command(filepath.Join(repo, "bin", "release"), args...)
	cmd.Dir = repo
	cmd.Env = append(testEnv(), "PATH="+ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()

	return string(out), err
}

func TestReleaseNextVersion(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected string
		how      string
	}{
		{name: "default bumps the fourth segment", args: []string{"--dry-run"}, expected: "v1.1.0.3", how: "build"},
		{name: "build bumps the fourth segment", args: []string{"build", "--dry-run"}, expected: "v1.1.0.3", how: "build"},
		{name: "patch bumps the third and resets the fourth", args: []string{"patch", "--dry-run"}, expected: "v1.1.1.0", how: "patch"},
		{name: "minor bumps the second and resets below", args: []string{"minor", "--dry-run"}, expected: "v1.2.0.0", how: "minor"},
		{name: "major bumps the first and resets below", args: []string{"major", "--dry-run"}, expected: "v2.0.0.0", how: "major"},
		{name: "explicit four-segment tag is taken as given", args: []string{"v3.0.1.4", "--dry-run"}, expected: "v3.0.1.4", how: "explicit"},
		{name: "explicit tag without the v prefix is normalised", args: []string{"3.0.1.4", "--dry-run"}, expected: "v3.0.1.4", how: "explicit"},
		{name: "explicit plain semver is accepted", args: []string{"v2.5.1", "--dry-run"}, expected: "v2.5.1", how: "explicit"},
	}

	repo := testRepo(t, "v1.0.0.7", "v1.1.0.0", "v1.1.0.1", "v1.1.0.2")
	ghDir := stubGH(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runRelease(t, repo, ghDir, tt.args...)
			require.NoError(t, err, out)
			assert.Contains(t, out, "current:  v1.1.0.2")
			assert.Contains(t, out, "release:  "+tt.expected+"  ("+tt.how+")")
			assert.Contains(t, out, "dry run")
		})
	}
}

func TestReleaseRejectsUnusableTags(t *testing.T) {
	tests := []struct {
		name          string
		arg           string
		expectedError string
	}{
		{name: "prerelease suffix sorts below the release it names", arg: "v1.2.0-rc1", expectedError: "Gem::Version"},
		{name: "five segments are not a tag shape CI builds", arg: "v1.2.0.0.1", expectedError: "vX.Y.Z.N"},
		{name: "two segments are not a tag shape CI builds", arg: "v1.2", expectedError: "vX.Y.Z.N"},
		{name: "a tag that already exists is never re-cut silently", arg: "v1.1.0.2", expectedError: "already exists"},
	}

	repo := testRepo(t, "v1.1.0.0", "v1.1.0.1", "v1.1.0.2")
	ghDir := stubGH(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runRelease(t, repo, ghDir, tt.arg, "--dry-run")
			require.Error(t, err, out)
			assert.Contains(t, out, tt.expectedError)
		})
	}
}

func TestReleaseListShowsEveryBump(t *testing.T) {
	repo := testRepo(t, "v1.0.0.7", "v1.1.0.0")
	ghDir := stubGH(t, "v1.0.0.7")

	out, err := runRelease(t, repo, ghDir, "list")
	require.NoError(t, err, out)

	assert.Contains(t, out, "current version: v1.1.0.0")
	assert.Contains(t, out, "build   v1.1.0.1")
	assert.Contains(t, out, "patch   v1.1.1.0")
	assert.Contains(t, out, "minor   v1.2.0.0")
	assert.Contains(t, out, "major   v2.0.0.0")
	assert.Contains(t, out, "v1.1.0.0", "the tag with no release is listed")
}

func TestReleaseRequiresAnExplicitVersionWithoutTags(t *testing.T) {
	repo := testRepo(t)
	ghDir := stubGH(t)

	out, err := runRelease(t, repo, ghDir, "--dry-run")
	require.Error(t, err, out)
	assert.Contains(t, out, "no v* tag")
}

func TestBackfillPlansOnlyTheTagsWithoutReleases(t *testing.T) {
	repo := testRepo(t, "v1.0.0.0", "v1.0.0.1", "v1.1.0.0", "v1.1.0.1")
	ghDir := stubGH(t, "v1.0.0.0", "v1.1.0.0")

	out, err := runRelease(t, repo, ghDir, "backfill", "--dry-run")
	require.NoError(t, err, out)

	assert.Contains(t, out, "v1.0.0.1")
	assert.Contains(t, out, "v1.1.0.1")
	assert.NotContains(t, out, "would create v1.0.0.0")
	assert.NotContains(t, out, "would create v1.1.0.0")
}

func TestBackfillNotesStartAtThePrecedingTag(t *testing.T) {
	repo := testRepo(t, "v1.0.0.0", "v1.0.0.1", "v1.1.0.0")
	ghDir := stubGH(t, "v1.0.0.0")

	out, err := runRelease(t, repo, ghDir, "backfill", "--dry-run")
	require.NoError(t, err, out)

	// Notes for a backfilled tag start at the tag before it in version order,
	// whether or not that tag has a release of its own.
	assert.Contains(t, out, "would create v1.0.0.1  notes from v1.0.0.0  (latest=false)")
	assert.Contains(t, out, "would create v1.1.0.0  notes from v1.0.0.1  (latest=true)")
}

func TestBackfillIsAnUpToDateNoop(t *testing.T) {
	repo := testRepo(t, "v1.0.0.0", "v1.0.0.1")
	ghDir := stubGH(t, "v1.0.0.0", "v1.0.0.1")

	out, err := runRelease(t, repo, ghDir, "backfill", "--dry-run")
	require.NoError(t, err, out)
	assert.Contains(t, out, "every tag already has a release")
}
