# CRDs for the parallax chart

This directory holds the `CustomResourceDefinition` manifests for the four
parallax CRDs — `Study`, `Trial`, `Plugin`, `Dataset` — that Helm installs
before any templated resource (Helm renders `crds/` first and never templates it).

**These files are generated, not hand-written.** They are produced from the
kubebuilder markers on the Go API types in `api/v1alpha1/` by `controller-gen`.

## Populating this directory

From the repo root:

```sh
make manifests
```

`make manifests` runs `controller-gen crd` over `./api/...`, writes the CRD YAML
into `config/crd/`, and copies it here (`charts/parallax/crds/`). Re-run it
whenever the API types change so the chart ships CRDs that match the operator.

Until `make manifests` has been run, this directory intentionally contains only
this README — the M0 scaffold does not commit generated artifacts.

## Install / upgrade notes

- Helm installs everything in `crds/` on `helm install` but **does not upgrade or
  delete them** on `helm upgrade`/`helm uninstall` (a Helm safety property).
  Upgrade CRDs explicitly with `kubectl apply -f charts/parallax/crds/` or
  `kubectl apply -k config/crd`.
- The `installCRDs` value governs CRDs managed out-of-band (GitOps): set it
  `false` and simply do not commit the generated CRD YAML here — the Helm-native
  `crds/` path only installs files that are actually present. (When CRDs are
  later generated, a maintainer may instead move them into a templated,
  `installCRDs`-gated file under `templates/`; the M0 scaffold keeps the simpler
  Helm-native `crds/` layout.)
