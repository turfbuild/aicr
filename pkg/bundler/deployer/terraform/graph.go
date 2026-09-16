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
	"regexp"
	"sort"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nonIdentifier matches every rune that cannot appear in a Terraform
// identifier. Component names are already constrained to safe path
// components by the time they reach here; this covers the hyphens and dots
// that are legal in a component name but not in a module label.
var nonIdentifier = regexp.MustCompile(`[^A-Za-z0-9_]`)

// moduleLabel converts a release name into a Terraform module label.
// Terraform identifiers must start with a letter or underscore, so a name
// that begins with a digit is prefixed rather than silently truncated.
//
// The mapping is not injective in principle — "gpu-operator" and
// "gpu.operator" would collide — so buildReleases checks the emitted labels
// for collisions instead of trusting this to be unique.
func moduleLabel(name string) string {
	label := nonIdentifier.ReplaceAllString(name, "_")
	if label == "" {
		return "_"
	}
	if c := label[0]; c >= '0' && c <= '9' {
		return "_" + label
	}
	return label
}

// release is one emitted folder rendered as one module call.
type release struct {
	// Folder is the localformat folder this module call installs.
	Folder localformat.Folder

	// Label is the module block label, e.g. "gpu_operator_pre".
	Label string

	// DependsOn lists the module labels this call must be ordered after,
	// sorted for deterministic output.
	DependsOn []string
}

// buildReleases turns the emitted folder list into module calls carrying the
// recipe's dependency DAG.
//
// Edges come from two places. WITHIN a component, folders chain in emission
// order — pre -> primary -> post -> readiness — because localformat emits them
// in the order they must apply and nothing else records that order. ACROSS
// components, each component's FIRST folder depends on the LAST folder of each
// declared dependencyRef, so a dependent waits for the dependency's whole
// chain and not merely its primary chart.
//
// Deriving the cross-component edges from folders rather than from component
// names is what keeps this package out of the business of re-deriving folder
// shape: it never has to know that a component with post-manifests grows a
// "-post" tail, only that the tail is the last folder carrying that Parent.
//
// A dependencyRef naming a component absent from refs — disabled via
// overrides.enabled=false, or satisfied externally — is dropped. Recipe
// resolution has already validated the real edges, and emitting
// depends_on = [module.absent] would not be a plan error, it would be a
// parse error.
//
// When serial is set the declared edges are replaced by a single chain: each
// component's first folder depends on the previous component's last folder,
// matching the flux deployer's --serial fallback.
func buildReleases(folders []localformat.Folder, refs []recipe.ComponentRef, serial bool) ([]release, error) {
	// first/last folder index per component, in emission order.
	firstOf := make(map[string]int, len(refs))
	lastOf := make(map[string]int, len(refs))
	for i, f := range folders {
		if _, seen := firstOf[f.Parent]; !seen {
			firstOf[f.Parent] = i
		}
		lastOf[f.Parent] = i
	}

	releases := make([]release, len(folders))
	labels := make(map[string]string, len(folders))
	for i, f := range folders {
		label := moduleLabel(f.Name)
		if prev, clash := labels[label]; clash {
			return nil, errIdentifierCollision(prev, f.Name, label)
		}
		labels[label] = f.Name
		releases[i] = release{Folder: f, Label: label}
	}

	// Within a component: chain each folder to the previous one.
	prevOfParent := make(map[string]int, len(refs))
	for i, f := range folders {
		if prev, ok := prevOfParent[f.Parent]; ok {
			releases[i].DependsOn = append(releases[i].DependsOn, releases[prev].Label)
		}
		prevOfParent[f.Parent] = i
	}

	// Across components: attach the declared edges to each component's head.
	present := make(map[string]bool, len(refs))
	for _, r := range refs {
		present[r.Name] = true
	}
	prevComponentTail := -1
	for _, ref := range refs {
		head, ok := firstOf[ref.Name]
		if !ok {
			// A component that emitted no folder (e.g. every manifest
			// rendered empty) is not an error here — localformat already
			// decided it contributes nothing to apply.
			continue
		}
		var targets []string
		if serial {
			if prevComponentTail >= 0 {
				targets = append(targets, releases[prevComponentTail].Label)
			}
		} else {
			for _, dep := range ref.DependencyRefs {
				if !present[dep] {
					continue
				}
				tail, hasTail := lastOf[dep]
				if !hasTail {
					continue
				}
				targets = append(targets, releases[tail].Label)
			}
		}
		releases[head].DependsOn = append(releases[head].DependsOn, targets...)
		prevComponentTail = lastOf[ref.Name]
	}

	for i := range releases {
		releases[i].DependsOn = dedupeSorted(releases[i].DependsOn)
	}
	return releases, nil
}

// dedupeSorted sorts and de-duplicates a label list. A component whose head
// folder is also its tail can pick up the same target twice when a dependency
// is named more than once; sorting keeps generation deterministic.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// valuesFilesForFolder returns the values files a folder's module call passes
// to helm, relative to the bundle root, in the order helm must layer them:
// values.yaml first, then cluster-values.yaml so operator edits win.
//
// localformat writes cluster-values.yaml unconditionally, empty when the
// component declares no dynamic paths. Referencing an empty one would be
// harmless but misleading — it reads as "there is something here to fill in" —
// so the second file is listed only when the component actually has dynamic
// values, matching the helmfile deployer's rule.
func valuesFilesForFolder(f localformat.Folder, dynamicPaths []string) []string {
	files := []string{f.Dir + "/" + fileValues}
	if len(dynamicPaths) > 0 {
		files = append(files, f.Dir+"/"+fileClusterValues)
	}
	return files
}
