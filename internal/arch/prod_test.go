// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// object is one Kubernetes object as YAML decodes it.
type object map[string]any

func (o object) kind() string { return str(o["kind"]) }
func (o object) name() string { return str(dig(o, "metadata", "name")) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// dig walks nested maps and answers nil at the first missing key.
func dig(v any, path ...string) any {
	for _, key := range path {
		if o, isObject := v.(object); isObject {
			v = map[string]any(o)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = m[key]
		if !ok {
			return nil
		}
	}
	return v
}

// list is a YAML sequence as a slice, nil for anything else.
func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// decodeDocs splits one YAML stream into its objects, skipping empty
// documents.
func decodeDocs(t *testing.T, name string, data []byte) []object {
	t.Helper()
	var out []object
	for i, doc := range bytes.Split(data, []byte("\n---")) {
		var o object
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatalf("%s: document %d does not parse: %v", name, i+1, err)
		}
		if len(o) > 0 {
			out = append(out, o)
		}
	}
	return out
}

// kustomization is the part of a kustomization.yaml the pure-Go load
// follows: what it includes, and which files its transformers name.
type kustomization struct {
	Resources []string `yaml:"resources"`
	Patches   []struct {
		Path string `yaml:"path"`
	} `yaml:"patches"`
	ConfigMapGenerator []struct {
		Name string   `yaml:"name"`
		Envs []string `yaml:"envs"`
	} `yaml:"configMapGenerator"`
	Images []struct {
		Name    string `yaml:"name"`
		NewName string `yaml:"newName"`
		NewTag  string `yaml:"newTag"`
	} `yaml:"images"`
}

// loadKustomization follows every resource of the kustomization at dir,
// checks that every file a patch or a generator names exists, and returns
// the objects the files carry, unpatched. It proves the tree is whole on a
// machine with no kubectl.
func loadKustomization(t *testing.T, dir string) (kustomization, []object) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	var k kustomization
	if err := yaml.Unmarshal(data, &k); err != nil {
		t.Fatalf("%s/kustomization.yaml: %v", dir, err)
	}
	var objects []object
	for _, ref := range k.Resources {
		path := filepath.Join(dir, filepath.FromSlash(ref))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s/kustomization.yaml names %s, which is not in the tree: %v", dir, ref, err)
		}
		if info.IsDir() {
			_, inner := loadKustomization(t, path)
			objects = append(objects, inner...)
			continue
		}
		file, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, decodeDocs(t, path, file)...)
	}
	for _, p := range k.Patches {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p.Path))); err != nil {
			t.Errorf("%s/kustomization.yaml patches %s, which is not in the tree", dir, p.Path)
		}
	}
	for _, g := range k.ConfigMapGenerator {
		for _, env := range g.Envs {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(env))); err != nil {
				t.Errorf("%s/kustomization.yaml generates %s from %s, which is not in the tree", dir, g.Name, env)
			}
		}
	}
	return k, objects
}

// kubectlRender runs kubectl kustomize over dir, or skips the test by name
// when kubectl is not on PATH, as it is not in the hermetic run.
func kubectlRender(t *testing.T, dir string) []object {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not on PATH, so the render is not checked here; the pure-Go load ran")
	}
	cmd := exec.Command("kubectl", "kustomize", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", dir, err, stderr.String())
	}
	return decodeDocs(t, dir, out)
}

// prodKinds is each kind deploy/prod may hold, with the API group and the
// resource the rollout identity's Role grants it under. A kind that is not
// here is cluster-scoped, a Secret, or an RBAC object, which a release must
// not apply: it belongs in deploy/prod-bootstrap.
var prodKinds = map[string][2]string{
	"Deployment":          {"apps", "deployments"},
	"ConfigMap":           {"", "configmaps"},
	"Service":             {"", "services"},
	"ServiceAccount":      {"", "serviceaccounts"},
	"Ingress":             {"networking.k8s.io", "ingresses"},
	"NetworkPolicy":       {"networking.k8s.io", "networkpolicies"},
	"PodDisruptionBudget": {"policy", "poddisruptionbudgets"},
}

// prodObjects is what deploy/prod carries, with the ConfigMap its generator
// makes.
func prodObjects(t *testing.T) (kustomization, []object) {
	t.Helper()
	k, objects := loadKustomization(t, filepath.Join(root(t), "deploy/prod"))
	if len(k.ConfigMapGenerator) > 0 {
		objects = append(objects, object{"kind": "ConfigMap", "metadata": map[string]any{"name": k.ConfigMapGenerator[0].Name}})
	}
	return k, objects
}

// prodResources is the set of group/resource pairs deploy/prod applies.
func prodResources(t *testing.T) []string {
	t.Helper()
	_, objects := prodObjects(t)
	var out []string
	for _, o := range objects {
		pair, ok := prodKinds[o.kind()]
		if !ok {
			t.Errorf("deploy/prod holds a %s, which the rollout identity does not cover and a release must not apply; keep it in deploy/prod-bootstrap or add it to this test and to rollout-identity.yaml", o.kind())
			continue
		}
		out = append(out, pair[0]+"/"+pair[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestRolloutIdentityCoversExactlyWhatProdApplies: the Role allows apply on
// the kinds deploy/prod holds and no others, never touches Secrets or Roles,
// and never deletes; the token Secret is bound to the account it names.
func TestRolloutIdentityCoversExactlyWhatProdApplies(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(root(t), "deploy/prod-bootstrap/rollout-identity.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var granted []string
	for _, o := range decodeDocs(t, "rollout-identity.yaml", data) {
		switch o.kind() {
		case "Role":
			for _, r := range list(o["rules"]) {
				for _, group := range list(dig(r, "apiGroups")) {
					for _, res := range list(dig(r, "resources")) {
						granted = append(granted, str(group)+"/"+str(res))
					}
				}
				for _, verb := range list(dig(r, "verbs")) {
					if !slices.Contains([]string{"get", "list", "create", "update", "patch", "watch"}, str(verb)) {
						t.Errorf("the Role grants the verb %v; a release applies, rolls and reads", verb)
					}
				}
			}
		case "Secret":
			if o.name() != "toposd-rollout-token" || str(o["type"]) != "kubernetes.io/service-account-token" ||
				str(dig(o, "metadata", "annotations", "kubernetes.io/service-account.name")) != "toposd-rollout" {
				t.Errorf("the token Secret %s is not toposd-rollout-token bound to toposd-rollout", o.name())
			}
		}
	}
	slices.Sort(granted)
	granted = slices.Compact(granted)
	if want := prodResources(t); !slices.Equal(granted, want) {
		t.Errorf("the Role grants %v, deploy/prod applies %v", granted, want)
	}
	text := string(data)
	for _, not := range []string{`"secrets"`, `"roles"`, `"rolebindings"`, `"namespaces"`, `"*"`, "ClusterRole"} {
		if strings.Contains(text, not) {
			t.Errorf("the rollout identity names %s; it holds no such grant", not)
		}
	}
	if !strings.Contains(text, `resources: ["deployments"]`+"\n"+`    verbs: ["watch"]`) {
		t.Error("the Role does not let the rollout watch the Deployment")
	}
}

// TestProdTreeKeepsToWhatAReleaseMayApply: what an operator applies is in
// deploy/prod-bootstrap, no Secret is in either tree beside the rollout
// identity's token, and every policy in deploy/prod selects the core's own
// Pods, since a policy over another service's Pods belongs to that service's
// tree.
func TestProdTreeKeepsToWhatAReleaseMayApply(t *testing.T) {
	prodResources(t)
	for _, dir := range []string{"deploy/prod", "deploy/prod-bootstrap"} {
		entries, err := os.ReadDir(filepath.Join(root(t), dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".yaml" || e.Name() == "kustomization.yaml" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root(t), dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range decodeDocs(t, e.Name(), data) {
				if o.kind() == "Secret" && (dir == "deploy/prod" || o.name() != "toposd-rollout-token") {
					t.Errorf("%s/%s holds the Secret %s; only the rollout identity's token Secret is in a tree, and only in deploy/prod-bootstrap", dir, e.Name(), o.name())
				}
				if dir == "deploy/prod" && (o.kind() == "Role" || o.kind() == "RoleBinding") {
					t.Errorf("deploy/prod holds the %s %s, which the rollout identity may not write", o.kind(), o.name())
				}
			}
		}
	}
	_, objects := prodObjects(t)
	for _, o := range objects {
		if o.kind() != "NetworkPolicy" {
			continue
		}
		sel, _ := dig(o, "spec", "podSelector", "matchLabels").(map[string]any)
		if len(sel) != 1 || sel["app.kubernetes.io/name"] != "toposd" {
			t.Errorf("deploy/prod holds the NetworkPolicy %s selecting %v; a policy over another service's Pods is that service's to apply", o.name(), sel)
		}
	}
}

// TestProdImageIsStampedByTheRelease: the images entry carries a tag in the
// one form release.stamp rewrites, and the stamp names that file.
func TestProdImageIsStampedByTheRelease(t *testing.T) {
	k, _ := prodObjects(t)
	if len(k.Images) != 1 || k.Images[0].Name != "toposd" || k.Images[0].NewName == "" {
		t.Fatalf("deploy/prod pins %v, want the one image toposd with a newName", k.Images)
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(k.Images[0].NewTag) {
		t.Errorf("the image tag %q is not a release tag", k.Images[0].NewTag)
	}
	cfg, err := os.ReadFile(filepath.Join(root(t), ".lateregate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "- file: deploy/prod/kustomization.yaml\n      pattern: 'newTag: v\\d+\\.\\d+\\.\\d+'") {
		t.Error(".lateregate.yaml has no release.stamp entry that moves newTag in deploy/prod/kustomization.yaml")
	}
}

// TestDeployJobDeploysWhatProdRuns: the deploy job passes the one image
// production runs under the name the overlay gives it, waits for every
// Deployment the overlay renders, checks the address the smoke script takes
// as its base, and leaves the publish and verify jobs running where the
// deploy is skipped.
func TestDeployJobDeploysWhatProdRuns(t *testing.T) {
	w, text := readRelease(t)
	job := w.Jobs["deploy"]
	if job.If != "${{ vars.TOPOS_RELEASE_DEPLOY != '' }}" {
		t.Errorf("deploy runs under %q, want the repository variable TOPOS_RELEASE_DEPLOY alone", job.If)
	}
	if !strings.Contains(text, "DEPLOY_KUBECONFIG: ${{ secrets.TOPOS_KUBECONFIG }}") {
		t.Error("deploy does not hand the stage the TOPOS_KUBECONFIG secret")
	}
	if !strings.HasPrefix(job.Uses, stagePrefix+"image-deploy.yml@") {
		t.Errorf("deploy calls %q, want the shared image-deploy stage", job.Uses)
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Errorf("deploy declares %v, want contents: read alone", job.Permissions)
	}
	if !slices.Equal(needs(job.Needs), []string{"build"}) {
		t.Errorf("deploy waits for %v, want build alone: no job proves the image before the release", needs(job.Needs))
	}
	if got, want := job.With["images"], "toposd=${{ needs.build.outputs.ref_1 }}@${{ needs.build.outputs.digest_1 }}"; got != want {
		t.Errorf("deploy passes the images %q, want %q: the image by digest, named as the overlay names it", got, want)
	}
	if _, ok := job.With["deploy_dir"]; ok {
		t.Error("deploy names a deploy_dir; the stage's default, deploy/prod, is the production tree")
	}
	if got := w.Jobs["publish"].If; got != "${{ always() && needs.build.result == 'success' && needs.deploy.result != 'failure' && needs.deploy.result != 'cancelled' }}" {
		t.Errorf("publish runs under %q, which a skipped deploy would turn into a skip or let a failed one through", got)
	}
	if got := w.Jobs["publish"].With["evidence"]; got != "${{ needs.deploy.result == 'success' && 'smoke-evidence' || '' }}" {
		t.Errorf("publish names the smoke evidence as %q", got)
	}
	if got := w.Jobs["verify"].If; got != "${{ always() && needs.publish.result == 'success' }}" {
		t.Errorf("verify runs under %q, which a skipped deploy would turn into a skip", got)
	}

	_, objects := prodObjects(t)
	var deployments []string
	for _, o := range objects {
		if o.kind() == "Deployment" {
			deployments = append(deployments, o.name())
		}
	}
	if waited := strings.Fields(job.With["deployments"]); !slices.Equal(waited, deployments) {
		t.Errorf("deploy waits for %v, deploy/prod renders the Deployments %v", waited, deployments)
	}
	// The address is the public base of the API, the origin and the base
	// path together, which tools/smoke/release.sh reads as BASE_URL.
	var ingress object
	for _, o := range objects {
		if o.kind() == "Ingress" {
			ingress = o
		}
	}
	if ingress == nil {
		t.Fatal("deploy/prod holds no Ingress")
	}
	rule := list(dig(ingress, "spec", "rules"))[0]
	path := str(dig(list(dig(rule, "http", "paths"))[0], "path"))
	if want := "https://" + str(dig(rule, "host")) + path; job.With["url"] != want {
		t.Errorf("deploy checks %q, the Ingress routes %s", job.With["url"], want)
	}
	// The rollout may wait for a node, so the stage waits as long as the
	// Deployment's own progress deadline.
	data, err := os.ReadFile(filepath.Join(root(t), "deploy/prod/deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var patch object
	if err := yaml.Unmarshal(data, &patch); err != nil {
		t.Fatal(err)
	}
	if got, want := job.With["rollout_timeout"], "15m"; got != want || dig(patch, "spec", "progressDeadlineSeconds") != uint64(900) {
		t.Errorf("deploy waits %q for a Deployment whose progress deadline is %v seconds, want %s for 900", got, dig(patch, "spec", "progressDeadlineSeconds"), want)
	}
}

// TestProdTakesTheImageTheStagePins runs the stage's own pin on a copy of
// the tree: after `kustomize edit set image <name>=<ref>@<digest>` in
// deploy/prod, the render of every Deployment runs that exact reference.
func TestProdTakesTheImageTheStagePins(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize is not on PATH, so the stage's pin is not exercised here")
	}
	dir := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join(root(t), "deploy"), dir).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ref := "ghcr.io/example/topos@sha256:" + strings.Repeat("0", 64)
	set := exec.Command("kustomize", "edit", "set", "image", "toposd="+ref)
	set.Dir = filepath.Join(dir, "deploy", "prod")
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("kustomize edit set image: %v\n%s", err, out)
	}
	n := 0
	for _, o := range kubectlRender(t, set.Dir) {
		if o.kind() != "Deployment" {
			continue
		}
		n++
		for _, c := range list(dig(o, "spec", "template", "spec", "containers")) {
			if got := str(dig(c, "image")); got != ref {
				t.Errorf("Deployment %s runs %q after the pin, want %q", o.name(), got, ref)
			}
		}
	}
	if n == 0 {
		t.Error("deploy/prod renders no Deployment")
	}
}
