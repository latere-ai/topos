# Deploy

Kustomize manifests for an installation of `toposd`, and the maintainer's
own installation. [`docs/install.md`](../docs/install.md) is the guide for
anyone installing the service.

| Directory | Holds |
|---|---|
| `base/` | the base of every installation: the Deployment, its Service and ServiceAccount, the NetworkPolicy around it, and the PodDisruptionBudget. It names no namespace and no image registry; an overlay names the namespace and points the placeholder image `toposd` at a published image |
| `bootstrap/` | the Namespace an installation runs in, applied once by hand |
| `examples/` | `generic/` is the smallest installation: the base in the namespace of `bootstrap/`, with the image, the public address and the issuer left as placeholders for you to replace |
| `prod/` | the maintainer's own installation, not an install path: the base with the maintainer's ingress, configuration, network policies and image pin. A release applies it through the `deploy` job of `.github/workflows/release.yml` only where the repository variable `TOPOS_RELEASE_DEPLOY` is set, so a fork deploys nothing. It holds only manifests that are safe to apply again |
| `prod-bootstrap/` | what the release never applies for that installation: the identity the `deploy` job runs as, applied once by an operator. Its [README](prod-bootstrap/README.md) says how |
