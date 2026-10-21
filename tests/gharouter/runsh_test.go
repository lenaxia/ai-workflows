// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// This file exercises scripts/opencode-cli/run.sh's install/staleness logic.
//
// run.sh replaced the anomalyco/opencode composite, whose cache was keyed on
// the CLI version — a version bump always produced a fresh install. run.sh's
// first draft only checked binary PRESENCE, which silently regressed that
// property on self-hosted runners with a persistent $HOME (gokore): after a
// bump, those runners kept executing the old binary forever. These tests
// pin the reinstall-on-drift behavior and its complement (no redundant
// download when the installed version matches).
package gharouter

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runShPath resolves scripts/opencode-cli/run.sh relative to the repo.
func runShPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "opencode-cli", "run.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("run.sh not found at %s: %v", p, err)
	}
	return p
}

// stubBinDir builds PATH shims for curl and tar that serve a fake tarball
// containing an `opencode` stub reporting the given version, and RECORD
// invocations to files under dir.
func stubBinDir(t *testing.T, dir, servedVersion string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	curl := `#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >> ` + filepath.Join(dir, "calls.txt") + `
# The fake tarball: a single 'opencode' stub at the root, as the real asset.
STUB=` + filepath.Join(dir, "opencode") + `
cat > "$STUB" <<'EOS'
#!/usr/bin/env bash
if [ "${1:-}" = "--version" ]; then echo "` + servedVersion + `"; exit 0; fi
echo "opencode github run executed with args: $*" >> ` + filepath.Join(dir, "calls.txt") + `
EOS
chmod +x "$STUB"
tar -czf /tmp/opencode.tar.gz -C ` + dir + ` opencode
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	tarStub := `#!/usr/bin/env bash
set -euo pipefail
printf 'tar %s\n' "$*" >> ` + filepath.Join(dir, "calls.txt") + `
exec /usr/bin/tar "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "tar"), []byte(tarStub), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runRunSh executes run.sh in the harness home with a pre-seeded binary (may
// be empty), returning combined output. The `exec` of the stub binary lands
// in calls.txt.
func runRunSh(t *testing.T, dir, home, seededVersion string) string {
	t.Helper()
	ocDir := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if seededVersion != "" {
		stub := "#!/usr/bin/env bash\nif [ \"${1:-}\" = \"--version\" ]; then echo \"" + seededVersion + "\"; exit 0; fi\necho \"opencode github run executed\" >> " + filepath.Join(dir, "calls.txt") + "\n"
		if err := os.WriteFile(filepath.Join(ocDir, "opencode"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", runShPath(t))
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin",
		"OC_RELEASE=opencode-v9.9.9-test.1",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run.sh exited %v; output:\n%s", err, out)
	}
	return string(out)
}

func calls(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls.txt"))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestRunShReinstallsOnVersionDrift: a pre-seeded binary reporting an OLD
// version must be re-downloaded when OC_RELEASE moved — the drift class the
// replaced composite's version-keyed cache prevented, and which silently
// defeats the bump procedure on persistent-$HOME self-hosted runners.
func TestRunShReinstallsOnVersionDrift(t *testing.T) {
	dir := t.TempDir()
	stubBinDir(t, dir, "opencode-v9.9.9-test.1") // tarball serves the NEW version
	home := filepath.Join(dir, "home")
	out := runRunSh(t, dir, home, "opencode-v0.0.1-ancient")

	c := calls(t, dir)
	if !strings.Contains(c, "curl ") {
		t.Errorf("stale installed binary did NOT trigger a re-download — OC_RELEASE drift is silently ignored on persistent runners.\noutput:\n%s\ncalls:\n%s", out, c)
	}
	if !strings.Contains(out, "opencode-v9.9.9-test.1") {
		t.Errorf("the executed/installed binary does not report the requested OC_RELEASE.\noutput:\n%s", out)
	}
}

// TestRunShFailsWhenFreshInstallReportsWrongVersion: after a fresh download,
// a binary not reporting OC_RELEASE must abort the run — a wrong/clobbered/
// stale release asset may not execute silently (the drift check establishes
// the version contract; this pins its enforcement on the install path).
func TestRunShFailsWhenFreshInstallReportsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	stubBinDir(t, dir, "opencode-v0.0.1-other") // tarball serves the WRONG version
	home := filepath.Join(dir, "home")

	ocDir := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", runShPath(t))
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin",
		"OC_RELEASE=opencode-v9.9.9-test.1",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("run.sh exited 0 although the freshly installed binary reports the wrong version — the post-install contract is unenforced.\noutput:\n%s", out)
	}
	if !strings.Contains(string(out), "::error::") {
		t.Errorf("expected a ::error:: diagnostic on post-install version mismatch; output:\n%s", out)
	}
}

// TestRunShSkipsInstallWhenCurrent: a binary already reporting OC_RELEASE
// must NOT re-download (~50MB per run for nothing). Guards against
// over-correcting the drift fix into an unconditional install.
func TestRunShSkipsInstallWhenCurrent(t *testing.T) {
	dir := t.TempDir()
	stubBinDir(t, dir, "opencode-v9.9.9-test.1")
	home := filepath.Join(dir, "home")
	runRunSh(t, dir, home, "opencode-v9.9.9-test.1")

	if c := calls(t, dir); strings.Contains(c, "curl ") {
		t.Errorf("run.sh re-downloaded although the installed binary already reports OC_RELEASE.\ncalls:\n%s", c)
	}
}

// TestTrustedBotAuthDefaultSetBehavior executes trusted-bot-auth.sh directly
// (pure jq decision, no gh needed) and pins the default-trusted contract:
// the propagation bot gets TOKEN even with an empty caller list; an
// unlisted bot never does; the caller list still extends the set.
func TestTrustedBotAuthDefaultSetBehavior(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "trusted-bot-auth.sh")

	run := func(t *testing.T, actor, list string) string {
		t.Helper()
		outDir := t.TempDir()
		envFile := filepath.Join(outDir, "github_env")
		outFile := filepath.Join(outDir, "github_output")
		cmd := exec.Command("bash", script)
		cmd.Env = append(os.Environ(),
			"ACTOR="+actor,
			"TRUSTED_APP_BOTS="+list,
			"GITHUB_TOKEN=jobtoken",
			"GITHUB_OUTPUT="+outFile,
			"GITHUB_ENV="+envFile,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script exited %v for actor %s: %s", err, actor, out)
		}
		return string(out)
	}

	t.Run("propagation bot default-trusted with empty list", func(t *testing.T) {
		out := run(t, "ai-workflows-propagation[bot]", "[]")
		if !strings.Contains(out, "trusted app bot") {
			t.Errorf("propagation bot must be admitted via the default set; output:\n%s", out)
		}
	})
	t.Run("unlisted bot refused with empty list", func(t *testing.T) {
		out := run(t, "some-other-app[bot]", "[]")
		if strings.Contains(out, "trusted app bot") {
			t.Errorf("an unlisted bot must NOT be admitted by the default set; output:\n%s", out)
		}
	})
	t.Run("caller list still extends the set", func(t *testing.T) {
		out := run(t, "my-app[bot]", `["my-app[bot]"]`)
		if !strings.Contains(out, "trusted app bot") {
			t.Errorf("a caller-listed bot must be admitted; output:\n%s", out)
		}
	})
	t.Run("malformed list still fails closed", func(t *testing.T) {
		out := run(t, "ai-workflows-propagation[bot]", "not-json")
		if strings.Contains(out, "trusted app bot") {
			t.Errorf("malformed caller JSON must fail CLOSED even for a default-trusted actor (the whole trust pipeline degrades, not just the caller list); output:\n%s", out)
		}
	})
}
