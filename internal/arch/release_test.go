// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"encoding/json/v2"
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
		"\nCOPY dist/topos-machine-* " + helpers + "\n",
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

// workflow is a GitHub Actions workflow, read for its jobs' order, the
// reusable workflows they call, their inputs and their permissions.
type workflow struct {
	Jobs map[string]struct {
		Needs       any               `yaml:"needs"`
		If          string            `yaml:"if"`
		Uses        string            `yaml:"uses"`
		With        map[string]string `yaml:"with"`
		Permissions map[string]string `yaml:"permissions"`
	} `yaml:"jobs"`
}

// readRelease is the release workflow, decoded.
func readRelease(t *testing.T) (workflow, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w, string(b)
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

// stagePrefix is where latere-ai/ci keeps the shared image release
// stages; a job that calls one names it as stagePrefix + "<file>@<ref>".
const stagePrefix = "latere-ai/ci/.github/workflows/"

// TestReleasePublishesUnderTheOwnersNamespace is spec 028's release
// order and namespace, read from the workflow. The build stage of
// latere-ai/ci, after the tag's commit passed verify, builds the image
// topos from Dockerfile.ci, pushes it by digest under the account that
// pushed the tag, lowered, and checks it reports the tag's version; no
// input names a namespace, so a fork's tag publishes under the fork. The
// publish stage then tags those digests and publishes the release, and
// the verify stage checks the published release. The deploy stage sits
// between the build and the publish stage and is skipped wherever
// TOPOS_RELEASE_DEPLOY is unset, so the publish and verify jobs name the
// results they wait on (TestDeployJobDeploysWhatProdRuns). All three come from one
// ref of ci, signing and tagging happen in the stages alone, and only the
// build and publish jobs hold a write permission.
func TestReleasePublishesUnderTheOwnersNamespace(t *testing.T) {
	w, text := readRelease(t)
	build, publish, verify := w.Jobs["build"], w.Jobs["publish"], w.Jobs["verify"]

	ref := ""
	for name, file := range map[string]string{"build": "image-build.yml", "publish": "image-publish.yml", "verify": "release-verify.yml"} {
		uses := w.Jobs[name].Uses
		at, ok := strings.CutPrefix(uses, stagePrefix+file+"@")
		if !ok || at == "" {
			t.Fatalf("the %s job calls %q, want %s%s@<ref>", name, uses, stagePrefix, file)
		}
		if ref == "" {
			ref = at
		} else if at != ref {
			t.Errorf("the stages come from different refs of ci: %s and %s", ref, at)
		}
	}
	if len(needs(build.Needs)) != 0 || !slices.Equal(needs(publish.Needs), []string{"build", "deploy"}) || !slices.Equal(needs(verify.Needs), []string{"publish"}) {
		t.Fatalf("the order is build after %v, publish after %v, verify after %v; want build first, then deploy beside it, then publish, then verify", build.Needs, publish.Needs, verify.Needs)
	}
	if v := build.With["verify_workflow"]; v != "" && v != "verify.yml" {
		t.Errorf("the build stage waits for %s, want the per-push verify.yml", v)
	}

	var image struct {
		Name          string `json:"name"`
		Dockerfile    string `json:"dockerfile"`
		VersionArgs   string `json:"version_args"`
		VersionExpect string `json:"version_expect"`
	}
	if err := json.Unmarshal([]byte(build.With["image_1"]), &image); err != nil {
		t.Fatalf("image_1 is not JSON: %v", err)
	}
	if image.Name != "topos" || image.Dockerfile != "Dockerfile.ci" {
		t.Errorf("the build stage builds %s from %s, want spec 028's image topos from Dockerfile.ci", image.Name, image.Dockerfile)
	}
	if image.VersionArgs != "-version" || image.VersionExpect != "toposd {tag} (" {
		t.Errorf("the candidate is asked %q and must print %q, want -version and %q", image.VersionArgs, image.VersionExpect, "toposd {tag} (")
	}
	if publish.With["images"] != "${{ needs.build.outputs.images }}" {
		t.Errorf("publish tags %q, want the digests the build stage pushed", publish.With["images"])
	}
	if verify.With["images"] != "topos" {
		t.Errorf("verify checks %q, want the image topos", verify.With["images"])
	}

	for name, job := range w.Jobs {
		if job.With["namespace"] != "" {
			t.Errorf("the %s job names the namespace %q; a fork's tag would publish there", name, job.With["namespace"])
		}
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "ghcr.io/latere") {
		t.Error("the workflow names a fixed owner's namespace; a fork's tag would publish there")
	}
	for _, own := range []string{"cosign", "anchore/sbom-action", "actions/attest-", "imagetools"} {
		if strings.Contains(lower, own) {
			t.Errorf("the workflow names %s; signing, attesting and tagging are the stages'", own)
		}
	}
	for name, job := range w.Jobs {
		writes := false
		for _, level := range job.Permissions {
			writes = writes || level == "write"
		}
		if writes != (name == "build" || name == "publish") {
			t.Errorf("the %s job holds %v", name, job.Permissions)
		}
	}
}

// helperPlatforms are the platforms a machine's helper is built for: a
// Cella sandbox's, and a person's own computer's, which may be a Mac
// (spec 080).
var helperPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}

// TestTheReleaseBuildsTheHelperForEveryPlatform is spec 080's release
// rule: the release workflow and the developer image each build
// topos-machine for every platform of helperPlatforms, under the name
// the server reads a build by, topos-machine-<os>-<arch>. The release
// writes the binaries into dist/, flat, where Dockerfile.ci copies them
// from and the publish stage attaches every file to the release.
func TestTheReleaseBuildsTheHelperForEveryPlatform(t *testing.T) {
	w, _ := readRelease(t)
	build := w.Jobs["build"].With["archives_command"]
	docker, err := os.ReadFile(filepath.Join(root(t), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"release.yml": build, "Dockerfile": string(docker)} {
		if !strings.Contains(text, "for platform in "+strings.Join(helperPlatforms, " ")) || !strings.Contains(text, "topos-machine-${os}-${arch}") && !strings.Contains(text, "topos-machine-$os-$arch") {
			t.Errorf("%s does not build the helper for %v as topos-machine-<os>-<arch>:\n%s", name, helperPlatforms, text)
		}
	}
	for _, out := range []string{`-o "dist/toposd_linux_${arch}" ./cmd/toposd`, `-o "dist/topos-machine-${os}-${arch}" ./cmd/topos-machine`} {
		if !strings.Contains(build, out) {
			t.Errorf("the release's archives command lacks %q, the path Dockerfile.ci copies from:\n%s", out, build)
		}
	}
}
