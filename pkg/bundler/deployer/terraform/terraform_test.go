// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package terraform

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testBundlerVersion = "v1.0.0"

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

// TestGenerate_Scenarios is the golden-file suite for Generator.Generate.
// Each row supplies a configured Generator, the testdata/<name>/ dir holding
// the expected output, and the files to byte-compare. Error paths and the
// structural invariants that no single golden can express live in their own
// tests below.
func TestGenerate_Scenarios(t *testing.T) {
	scenarios := []struct {
		name    string
		gen     *Generator
		goldens []string
	}{
		{
			// The base case, and the one that locks the graph: three
			// components where gpu-operator declares both of the others.
			// Both the concurrency claim (nfd and cert-manager carry no
			// depends_on) and the module-call placement of the edge are
			// visible in this golden.
			name: "upstream_helm",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"cert-manager", "nfd"}
				return &Generator{
					RecipeResult: recipeWith(cm, nfd, gpu),
					ComponentValues: map[string]map[string]any{
						"cert-manager": {"crds": map[string]any{"enabled": true}},
						"gpu-operator": {"driver": map[string]any{"enabled": false}},
					},
					Version: testBundlerVersion,
				}
			}(),
			goldens: []string{
				fileMain, fileVersions, fileVars, fileOutputs,
				filepath.Join(moduleDir, "main.tf"),
				filepath.Join(moduleDir, "variables.tf"),
			},
		},
		{
			// A component with post-manifests expands into two releases.
			// The dependent must wait for the TAIL of that chain, not for
			// the primary chart, or it races the manifests.
			name: "post_manifest_tail",
			gen: func() *Generator {
				crds := ref("agentgateway-crds", "agentgateway-system", "agentgateway-crds", "v1.5.0", "oci://cr.agentgateway.dev/charts")
				gw := ref("agentgateway", "agentgateway-system", "agentgateway", "v1.5.0", "oci://cr.agentgateway.dev/charts")
				gw.DependencyRefs = []string{"agentgateway-crds"}
				return &Generator{
					RecipeResult: recipeWith(crds, gw),
					Version:      testBundlerVersion,
					ComponentPostManifests: map[string]map[string][]byte{
						"agentgateway-crds": {"gateway-api-crds.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: placeholder\n")},
					},
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// --serial discards the declared graph for one linear chain.
			name: "serial",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				nfd := ref("nfd", "node-feature-discovery", "node-feature-discovery", "0.18.1", "https://kubernetes-sigs.github.io/node-feature-discovery/charts")
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				gpu.DependencyRefs = []string{"cert-manager", "nfd"}
				return &Generator{
					RecipeResult: recipeWith(cm, nfd, gpu),
					Version:      testBundlerVersion,
					Serial:       true,
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// The containment shim adds a resource and a lifecycle block to
			// the shared module, and one more argument to every call.
			name: "cluster_rollover",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult:    recipeWith(cm),
					Version:         testBundlerVersion,
					ClusterRollover: true,
				}
			}(),
			goldens: []string{
				fileMain, fileVersions,
				filepath.Join(moduleDir, "main.tf"),
				filepath.Join(moduleDir, "variables.tf"),
			},
		},
		{
			// A component the shared override table marks asynchronous
			// carries literal wait/timeout arguments instead of the
			// bundle-wide variables, with the reason inline. Rendering it
			// as var.wait would let an operator silently re-enable a wait
			// that is known to time out.
			name: "async_component",
			gen: func() *Generator {
				gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
				kai := ref("kai-scheduler", "kai-scheduler", "kai-scheduler", "v0.16.9", "oci://ghcr.io/kai-scheduler/kai-scheduler")
				kai.DependencyRefs = []string{"gpu-operator"}
				return &Generator{
					RecipeResult: recipeWith(gpu, kai),
					Version:      testBundlerVersion,
				}
			}(),
			goldens: []string{fileMain},
		},
		{
			// Child-module form: no provider block, no connection
			// variables, no tfvars example. The rollover shim's trigger
			// moves to cluster_endpoint, since a child module has no
			// cluster_host of its own.
			name: "child_module",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult:    recipeWith(cm),
					Version:         testBundlerVersion,
					ChildModule:     true,
					ClusterRollover: true,
				}
			}(),
			goldens: []string{fileMain, fileVersions, fileVars},
		},
		{
			// A component with dynamic value paths gets cluster-values.yaml
			// layered after values.yaml, so operator edits win.
			name: "dynamic_values",
			gen: func() *Generator {
				cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
				return &Generator{
					RecipeResult: recipeWith(cm),
					ComponentValues: map[string]map[string]any{
						"cert-manager": {"replicaCount": 1, "global": map[string]any{"imagePullSecrets": []any{}}},
					},
					DynamicValues: map[string][]string{"cert-manager": {"global.imagePullSecrets"}},
					Version:       testBundlerVersion,
				}
			}(),
			goldens: []string{fileMain},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			outputDir := t.TempDir()
			out, err := sc.gen.Generate(context.Background(), outputDir)
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if out == nil || len(out.Files) == 0 {
				t.Fatalf("Generate() returned no files")
			}
			// Every scenario's main.tf must be fmt-stable, not only the
			// ones whose goldens happen to be checked in.
			assertFmtStable(t, filepath.Join(outputDir, fileMain))
			for _, rel := range sc.goldens {
				assertGolden(t, outputDir, filepath.Join("testdata", sc.name), rel)
			}
			// A child module is configured by its caller's module block, so
			// a tfvars example would be a file nothing can read.
			_, statErr := os.Stat(filepath.Join(outputDir, fileTfvars))
			if sc.gen.ChildModule && statErr == nil {
				t.Errorf("child-module bundle emitted %s", fileTfvars)
			}
			if !sc.gen.ChildModule && statErr != nil {
				t.Errorf("root-module bundle is missing %s: %v", fileTfvars, statErr)
			}
		})
	}
}

// TestGenerate_EdgeToDisabledComponentIsDropped locks the rule that keeps a
// generated bundle parseable: an edge to a component that was filtered out
// must disappear, because `depends_on = [module.absent]` is not a plan-time
// failure the operator can debug, it is a parse error in output AICR reported
// as successfully generated.
func TestGenerate_EdgeToDisabledComponentIsDropped(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	cm.Overrides = map[string]any{"enabled": false}
	gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
	gpu.DependencyRefs = []string{"cert-manager"}

	g := &Generator{RecipeResult: recipeWith(cm, gpu), Version: testBundlerVersion}
	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	main := readFile(t, filepath.Join(outputDir, fileMain))
	if strings.Contains(main, "cert_manager") {
		t.Errorf("main.tf references the disabled component:\n%s", main)
	}
	// "depends_on" also appears in the generated header comment, so match
	// the argument itself.
	if strings.Contains(main, "depends_on = [") {
		t.Errorf("main.tf kept a depends_on with no surviving target:\n%s", main)
	}
}

func TestGenerate_RejectsKustomizeComponent(t *testing.T) {
	k := ref("my-kustomization", "default", "", "", "https://github.com/example/repo")
	k.Type = recipe.ComponentTypeKustomize

	g := &Generator{RecipeResult: recipeWith(k), Version: testBundlerVersion}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Generate() accepted a Kustomize component")
	}
	if !strings.Contains(err.Error(), "Helm components only") {
		t.Errorf("error does not name the limitation: %v", err)
	}
}

func TestGenerate_RejectsCyclicGraph(t *testing.T) {
	a := ref("a", "default", "a", "1.0.0", "https://example.com/charts")
	b := ref("b", "default", "b", "1.0.0", "https://example.com/charts")
	a.DependencyRefs = []string{"b"}
	b.DependencyRefs = []string{"a"}

	g := &Generator{RecipeResult: recipeWith(a, b), Version: testBundlerVersion}
	if _, err := g.Generate(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Generate() accepted a cyclic dependency graph")
	}
}

func TestGenerate_RejectsNilRecipeResult(t *testing.T) {
	g := &Generator{Version: testBundlerVersion}
	if _, err := g.Generate(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Generate() accepted a nil RecipeResult")
	}
}

func TestGenerate_RespectsCancelledContext(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	g := &Generator{RecipeResult: recipeWith(cm), Version: testBundlerVersion}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Generate(ctx, t.TempDir()); err == nil {
		t.Fatal("Generate() ignored a cancelled context")
	}
}

func TestModuleLabel(t *testing.T) {
	cases := map[string]string{
		"gpu-operator":      "gpu_operator",
		"nvidia.dra.driver": "nvidia_dra_driver",
		"cert-manager-post": "cert_manager_post",
		"1password-connect": "_1password_connect",
		"already_fine":      "already_fine",
	}
	for in, want := range cases {
		if got := moduleLabel(in); got != want {
			t.Errorf("moduleLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildReleases_LabelCollision covers the one way the name -> label
// mapping can lose information. Two component names that differ only in a
// separator collapse to the same module label, which terraform reports as a
// duplicate block in a file AICR wrote; catching it here attributes it to the
// recipe instead.
func TestBuildReleases_LabelCollision(t *testing.T) {
	a := ref("gpu-operator", "default", "gpu-operator", "1.0.0", "https://example.com/charts")
	b := ref("gpu.operator", "default", "gpu-operator", "1.0.0", "https://example.com/charts")

	g := &Generator{RecipeResult: recipeWith(a, b), Version: testBundlerVersion}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Generate() accepted two components that map to the same module label")
	}
	if !strings.Contains(err.Error(), "module label") {
		t.Errorf("error does not explain the collision: %v", err)
	}
}

func TestHCLString(t *testing.T) {
	cases := map[string]string{
		`plain`:             `"plain"`,
		`with "quotes"`:     `"with \"quotes\""`,
		`back\slash`:        `"back\\slash"`,
		`${var.evil}`:       `"$${var.evil}"`,
		`%{ if true }`:      `"%%{ if true }"`,
		"tab\tand\nnewline": `"tab\tand\nnewline"`,
	}
	for in, want := range cases {
		if got := hclString(in); got != want {
			t.Errorf("hclString(%q) = %s, want %s", in, got, want)
		}
	}
}

// attrLine matches a single `name = value` argument line, capturing the
// indent and the name so assertFmtStable can check the column the `=` landed
// in without parsing HCL.
var attrLine = regexp.MustCompile(`^(\s*)([A-Za-z_][A-Za-z0-9_]*)( +)= `)

// assertFmtStable verifies that every contiguous run of argument lines has
// its `=` aligned to exactly one space past the longest name in the run —
// which is what `terraform fmt` produces, and therefore what makes generated
// output byte-identical to committed output.
//
// A run ends at any line that is not a simple argument (a blank line, a
// closing brace, a list element), matching fmt's own reset rule. Lines inside
// multi-line values are skipped for the same reason: they are not arguments.
func assertFmtStable(t *testing.T, path string) {
	t.Helper()
	var run []struct {
		lineNo int
		indent string
		name   string
		eqCol  int
	}
	flush := func() {
		if len(run) == 0 {
			return
		}
		want := 0
		for _, l := range run {
			if n := len(l.indent) + len(l.name); n > want {
				want = n
			}
		}
		want++ // the single space before '='
		for _, l := range run {
			if l.eqCol != want {
				t.Errorf("%s:%d: %q has '=' at column %d, terraform fmt would put it at %d",
					filepath.Base(path), l.lineNo, l.name, l.eqCol, want)
			}
		}
		run = run[:0]
	}

	for i, line := range strings.Split(readFile(t, path), "\n") {
		m := attrLine.FindStringSubmatch(line)
		if m == nil {
			flush()
			continue
		}
		run = append(run, struct {
			lineNo int
			indent string
			name   string
			eqCol  int
		}{i + 1, m[1], m[2], len(m[1]) + len(m[2]) + len(m[3])})
	}
	flush()
}

func ref(name, ns, chart, version, source string) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name:      name,
		Namespace: ns,
		Chart:     chart,
		Version:   version,
		Source:    source,
		Type:      recipe.ComponentTypeHelm,
	}
}

func recipeWith(refs ...recipe.ComponentRef) *recipe.RecipeResult {
	r := &recipe.RecipeResult{}
	r.Metadata.Version = testBundlerVersion
	r.ComponentRefs = refs
	order := make([]string, 0, len(refs))
	for _, ref := range refs {
		order = append(order, ref.Name)
	}
	r.DeploymentOrder = order
	return r
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err = os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err = os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", relPath, got, want)
	}
}
