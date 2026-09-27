// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/internal/config"
)

// The markers that fence the runtime stage both images share.
const (
	stageOpen  = "# >>> shared runtime base <<<"
	stageClose = "# <<< shared runtime base >>>"
)

// runtimeStage is the text between the two markers of one Dockerfile. A
// marker that is missing or repeated is a failure and not an empty stage,
// because an empty stage would compare equal to another empty one.
func runtimeStage(t *testing.T, name, text string) string {
	t.Helper()
	if strings.Count(text, stageOpen) != 1 || strings.Count(text, stageClose) != 1 {
		t.Fatalf("%s: the runtime stage markers appear exactly once each", name)
	}
	_, rest, _ := strings.Cut(text, stageOpen)
	stage, _, _ := strings.Cut(rest, stageClose)
	return stage
}

// port is the port of a listen address, such as ":8080".
func port(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheReleaseImageSharesTheDevelopersRuntime is spec 028's image
// rule: Dockerfile and Dockerfile.ci are byte-identical between the
// markers, the stage is the distroless static base running as nonroot
// with both listeners' default ports exposed, and the release file has no
// build stage, so a released image differs from a developer's in where
// the binaries came from and in nothing else. Both put toposd and the
// helper builds where the server looks for them by default.
func TestTheReleaseImageSharesTheDevelopersRuntime(t *testing.T) {
	dir := root(t)
	files := map[string]string{}
	for _, name := range []string{"Dockerfile", "Dockerfile.ci"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	stage := runtimeStage(t, "Dockerfile", files["Dockerfile"])
	if got := runtimeStage(t, "Dockerfile.ci", files["Dockerfile.ci"]); got != stage {
		t.Errorf("the runtime stages differ:\nDockerfile:%s\nDockerfile.ci:%s", stage, got)
	}
	expose := "\nEXPOSE " + port(t, config.DefaultPublicAddr) + " " + port(t, config.DefaultInternalAddr) + "\n"
	for _, want := range []string{"\nFROM gcr.io/distroless/static-debian12:nonroot\n", expose, "\nUSER nonroot:nonroot\n"} {
		if !strings.Contains(stage, want) {
			t.Errorf("the shared stage lacks %q", strings.TrimSpace(want))
		}
	}
	release := files["Dockerfile.ci"]
	if strings.Contains(release, "\nRUN ") || strings.Contains(release, "FROM golang") {
		t.Error("Dockerfile.ci has a build stage; a release image copies the binaries the pipeline built")
	}
	helpers := strings.TrimRight(config.DefaultMachineHelpers, "/") + "/"
	for _, want := range []string{
		"\nARG TARGETARCH\nCOPY dist/toposd_linux_${TARGETARCH} /usr/local/bin/toposd\n",
		"\nCOPY dist/helpers/ " + helpers + "\n",
	} {
		if !strings.Contains(release, want) {
			t.Errorf("Dockerfile.ci lacks %q", strings.TrimSpace(want))
		}
	}
	if !strings.Contains(files["Dockerfile"], "\nCOPY --from=build /out/helpers/ "+helpers+"\n") {
		t.Errorf("Dockerfile puts the helper builds elsewhere than %s", helpers)
	}
	for name, text := range files {
		if !strings.HasSuffix(text, "\nENTRYPOINT [\"/usr/local/bin/toposd\"]\n") {
			t.Errorf("%s does not end with the entry point /usr/local/bin/toposd", name)
		}
	}
}

// workflow is a GitHub Actions workflow, read for its jobs' order, their
// permissions and their commands.
type workflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Needs       any               `yaml:"needs"`
		Uses        string            `yaml:"uses"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// needs is a job's needs as a list, whether written as one name or many.
func needs(v any) []string {
	switch n := v.(type) {
	case string:
		return []string{n}
	case []any:
		var out []string
		for _, s := range n {
			if name, ok := s.(string); ok {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

// TestReleasePublishesUnderTheOwnersNamespace is spec 028's release
// order and namespace, read from the workflow: the image is pushed under
// the account that pushed the tag, lowered, and no fixed owner; it is
// pushed by digest in the job that builds it, after the tag's commit
// passed verify, and tagged only in a later job; the GitHub release comes
// last, from the family's shared notes pipeline. Only the jobs that write
// packages or contents hold that permission.
func TestReleasePublishesUnderTheOwnersNamespace(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.Env["IMAGE"] != "topos" || w.Env["REGISTRY"] != "ghcr.io" {
		t.Fatalf("the workflow publishes %s/%s, want spec 028's image topos", w.Env["REGISTRY"], w.Env["IMAGE"])
	}
	build, publish, release := w.Jobs["build"], w.Jobs["publish"], w.Jobs["release"]
	if !slices.Equal(needs(build.Needs), []string{"gate-green"}) || !slices.Equal(needs(publish.Needs), []string{"build"}) || !slices.Equal(needs(release.Needs), []string{"publish"}) {
		t.Fatalf("the order is build after %v, publish after %v, release after %v; want gate-green, build, publish", build.Needs, publish.Needs, release.Needs)
	}
	var owner, pushed, tagged bool
	for _, s := range build.Steps {
		owner = owner || (strings.Contains(s.Run, "${OVERRIDE:-$GITHUB_REPOSITORY_OWNER}") && strings.Contains(s.Run, "tr '[:upper:]' '[:lower:]'"))
		pushed = pushed || (strings.Contains(s.Run, "-f Dockerfile.ci") && strings.Contains(s.Run, "name=${REGISTRY}/${OWNER}/${IMAGE},push-by-digest=true"))
		if strings.Contains(s.Run, "-t ") || strings.Contains(s.Run, "imagetools create") {
			t.Errorf("the build job tags the image in %q; only publish tags it", s.Name)
		}
	}
	for _, s := range publish.Steps {
		tagged = tagged || strings.Contains(s.Run, `imagetools create -t "${REGISTRY}/${OWNER}/${IMAGE}:${GITHUB_REF_NAME}"`)
	}
	if !owner || !pushed || !tagged {
		t.Errorf("the namespace is the owner's: %v; the build pushes Dockerfile.ci by digest: %v; publish tags the digest with the tag: %v", owner, pushed, tagged)
	}
	if strings.Contains(strings.ToLower(string(b)), "ghcr.io/latere") {
		t.Error("the workflow names a fixed owner's namespace; a fork's tag would publish there")
	}
	if !strings.HasPrefix(release.Uses, "latere-ai/ci/.github/workflows/notes-release.yml@") {
		t.Errorf("the release job is %q, want the shared notes pipeline", release.Uses)
	}
	for name, job := range w.Jobs {
		writes := job.Permissions["packages"] == "write" || job.Permissions["contents"] == "write"
		if writes != (name == "build" || name == "publish" || name == "release") {
			t.Errorf("the %s job holds %v", name, job.Permissions)
		}
	}
}
