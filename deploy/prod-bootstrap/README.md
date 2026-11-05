# Latere's production bootstrap

These are the objects of Latere's installation that the release pipeline
never applies. `deploy/prod` is what a release applies, on every release, so
everything in it must be applied again without harm; the objects here cannot
be. [`deploy/bootstrap`](../bootstrap/namespace.yaml) is the namespace for
anyone installing the core and is not this directory.

| File | What it is | Why an operator applies it |
|---|---|---|
| `rollout-identity.yaml` | the ServiceAccount `toposd-rollout`, its Role and RoleBinding in the `latere` namespace over the kinds `deploy/prod` contains, and the token Secret `toposd-rollout-token` | a token Secret is minted once, and an identity that could rewrite its own Role could grant itself anything |

Four more things the core reads are not in any tree: the Secrets `toposd`
(the bearers), `toposd-identity` (the host client's secret),
`topos-core-db` (the database URLs) and `platformd-internal-ca`. They are
created by an operator and keep their names.

## The rollout identity

A kind added to `deploy/prod` is added to the Role of `rollout-identity.yaml`
and applied again by an operator. To create the identity and the credential
the release deploys with:

```sh
kubectl apply -f deploy/prod-bootstrap/rollout-identity.yaml

NS=latere SA=toposd-rollout
kubeconfig=$(mktemp) && chmod 600 "$kubeconfig"
KUBECONFIG="$kubeconfig" kubectl config set-cluster latere-k8s \
  --server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
KUBECONFIG="$kubeconfig" kubectl config set clusters.latere-k8s.certificate-authority-data \
  "$(kubectl -n "$NS" get secret "$SA-token" -o jsonpath='{.data.ca\.crt}')"
KUBECONFIG="$kubeconfig" kubectl config set-credentials "$SA" \
  --token="$(kubectl -n "$NS" get secret "$SA-token" -o jsonpath='{.data.token}' | base64 -d)"
KUBECONFIG="$kubeconfig" kubectl config set-context "$SA" \
  --cluster=latere-k8s --user="$SA" --namespace="$NS"
KUBECONFIG="$kubeconfig" kubectl config use-context "$SA"

# The server-side dry run authorizes every object a release applies without
# changing any, so a Role that misses a kind fails here and not in a release.
KUBECONFIG="$kubeconfig" kubectl apply -k deploy/prod --dry-run=server

gh secret set TOPOS_KUBECONFIG --repo latere-ai/topos < "$kubeconfig"
rm "$kubeconfig"
gh variable set TOPOS_RELEASE_DEPLOY --repo latere-ai/topos --body 1
```

The `deploy` job of `.github/workflows/release.yml` runs only while the
repository variable `TOPOS_RELEASE_DEPLOY` is set, so a fork, which sets
neither it nor the secret, publishes every artifact and deploys nothing.
Unsetting the variable turns the deploy off again.

Rotating the credential is deleting the token Secret, applying the identity
file again, and running the same steps.
