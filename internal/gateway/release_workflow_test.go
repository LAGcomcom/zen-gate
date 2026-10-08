package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release pipeline is the one part of this project that cannot be tried
// locally: a wrong Go version, a stripped binary or a second run of the publish
// step only shows up on the tag. These checks read the real files.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// 火绒 deletes a freshly written Go exe that has had its symbol table stripped,
// within about a minute of it appearing — so the shipped zen-gate.exe must not
// be built with -s -w, and the README must not tell users to build it that way.
func TestReleaseKeepsGoSymbolsInWindowsBinaries(t *testing.T) {
	for _, f := range []struct{ name, text string }{
		{".github/workflows/release.yml", repoFile(t, ".github/workflows/release.yml")},
		{"README.md", repoFile(t, "README.md")},
		{"tools/release.ps1", repoFile(t, "tools/release.ps1")},
	} {
		for i, line := range strings.Split(f.text, "\n") {
			if strings.Contains(line, "go build") && strings.Contains(line, "-s -w") {
				t.Errorf("%s:%d strips symbols; the exe is deleted by antivirus:\n%s", f.name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The Windows job pinned an older toolchain than go.mod requires, so the build
// only worked because setup-go silently fetched a newer one.
func TestWorkflowGoVersionMatchesModule(t *testing.T) {
	goMod := repoFile(t, "go.mod")
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(goMod)
	if m == nil {
		t.Fatal("no go directive in go.mod")
	}
	want := m[1]
	yml := repoFile(t, ".github/workflows/release.yml")
	found := regexp.MustCompile(`go-version: "?(\d+\.\d+)"?`).FindAllStringSubmatch(yml, -1)
	if len(found) == 0 {
		t.Fatal("no setup-go step in the release workflow")
	}
	for _, f := range found {
		if f[1] != want {
			t.Errorf("a job pins go-version %q but go.mod requires %q", f[1], want)
		}
	}
}

// Re-running a tag used to fail: gh release create exits non-zero when the
// release already exists, so the retry could never publish the binaries.
func TestReleaseStepIsIdempotent(t *testing.T) {
	for _, f := range []struct{ name, text string }{
		{".github/workflows/release.yml", repoFile(t, ".github/workflows/release.yml")},
		{"tools/release.ps1", repoFile(t, "tools/release.ps1")},
	} {
		if !strings.Contains(f.text, "gh release create") {
			t.Fatalf("%s no longer publishes a release", f.name)
		}
		if !strings.Contains(f.text, "gh release view") || !strings.Contains(f.text, "--clobber") {
			t.Errorf("%s: publishing a tag twice fails instead of re-uploading — guard gh release create with gh release view and upload --clobber", f.name)
		}
	}
}

// master requires the build checks, but the Windows job compiled without running
// the suite, so a PR could be green with failing tests.
func TestBuildJobRunsTests(t *testing.T) {
	yml := repoFile(t, ".github/workflows/release.yml")
	build := yml
	if i := strings.Index(yml, "build-macos:"); i > 0 {
		build = yml[:i]
	}
	if !regexp.MustCompile(`go (test|vet) \./`).MatchString(build) {
		t.Error("the Windows build job neither vets nor tests; it only compiles")
	}
}
