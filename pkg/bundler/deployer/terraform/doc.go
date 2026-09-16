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

/*
Package terraform generates a Terraform/OpenTofu root module from an AICR recipe.

Per-component folder content (values.yaml, cluster-values.yaml, upstream.env,
Chart.yaml + templates/) is delegated to pkg/bundler/deployer/localformat, the
same writer the helm and helmfile deployers use. This package owns only the
Terraform layer on top: a root module whose module calls mirror the emitted
folders one-for-one, plus the generic component module they all share.

# Deployment Ordering

Terraform's depends_on is a native DAG, so the recipe's declared dependencyRefs
project onto it exactly — the same fidelity the flux deployer gets from
dependsOn, and strictly more than the tier-based argocd (integer sync-waves)
and helmfile (nested level files) deployers, which can only approximate the
graph as depth tiers and therefore make every component wait on every sibling
at the prior depth.

Edges are derived from the emitted folder list rather than re-walked from the
recipe, so the -pre / primary / -post / -readiness chain a component may expand
into is ordered correctly without this package re-deriving folder shape:

  - within a component, each folder depends on the previous folder of the same
    component (pre -> primary -> post -> readiness);
  - the first folder of a component depends on the LAST folder of each of its
    declared dependencyRefs, so a dependent waits for the full chain;
  - a dependencyRef naming a component that is not in the enabled-filtered set
    (disabled via overrides, or provided externally) is dropped rather than
    generating an edge to a module that will not exist.

Under Serial (--serial) the cross-component edges collapse to a single chain:
the first folder of each component depends on the last folder of the previous
component in deployment order, so releases apply strictly one at a time.

# Why depends_on on the module call

Each component is one module call, and the edge is placed on the CALL, not on
the helm_release inside it. A depends_on on a module call is a fact about
everything the module contains, so the ordering still holds if a component's
module later grows a second resource. It also means a reader of main.tf sees
the recipe graph without opening the module.

# Cluster connection

The generated root module does NOT create a cluster. It configures the helm
provider from variables, so the bundle applies against whatever cluster the
operator points it at:

  - var.kubeconfig_path / var.kube_context for an existing kubeconfig (default);
  - var.cluster_host / var.cluster_ca_certificate / var.cluster_token when the
    cluster is created in the same configuration and its attributes are only
    known after apply.

The second form is the interesting one: with the connection unknown at plan
time, every helm_release in the bundle is unplannable in the first round. An
engine with deferred actions (OpenTofu's, Terraform's -allow-deferral, or
Turf) converges it in two rounds; stock terraform apply requires the cluster to
exist first. See README.md in the generated bundle.

# Cluster rollover

With ClusterRollover set, each component module additionally carries a
null_resource keyed on var.cluster_endpoint and a replace_triggered_by pointing
at it, so replacing the cluster replaces every release rather than leaving
state that describes objects in a cluster that no longer exists. Off by
default: the combination of replace_triggered_by and a deferred referent is
refused by current Terraform deferred-action builds ("no change found for
null_resource.cluster in module.X"), so the shim and the unknown-connection
path above cannot both be used on that engine.

# Component Type Support

Only Helm components are supported. Kustomize components produce an
ErrCodeInvalidRequest at generation time, matching the flux deployer.

# Generated Structure

	output/
	├── main.tf                     # one module call per folder, carrying the DAG
	├── versions.tf                 # required_providers + the helm provider config
	├── variables.tf
	├── outputs.tf
	├── terraform.tfvars.example
	├── modules/
	│   └── component/
	│       ├── main.tf             # the generic one-release module
	│       ├── variables.tf
	│       └── outputs.tf
	├── 001-nfd/                    # localformat folders, unchanged
	│   ├── values.yaml
	│   └── upstream.env
	├── 002-cert-manager/
	│   └── ...
	├── README.md
	└── checksums.txt               # optional
*/
package terraform
