# Release Process

DRANET releases both a container image and a Helm chart as OCI artifacts, both
promoted through the [Kubernetes image promotion pipeline](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/README.md) and this includes:

- Container image: `registry.k8s.io/networking/dranet:v1.1.0`
- Helm chart: `registry.k8s.io/networking/charts/dranet:v1.1.0`

## Overview of the release process (major/minor release):

1. Push a release tag (e.g. `v1.1.0`)
2. Generate release notes
3. Cloud Build builds & pushes staging artifacts
4. Retrieve artifact digests from staging
5. Promote artifacts via PR to `k8s.io`
6. Verify artifacts are live at `registry.k8s.io`
7. Publish GitHub Release

## 1. Push a release tag

Someone with write access to the repository creates and pushes a tag on the release branch:

```bash
git checkout release-1.1
git tag -a v1.1.0 -m "Release v1.1.0"
git push origin v1.1.0
```

This triggers the Prow postsubmit job [post-dranet-image](https://github.com/kubernetes/test-infra/blob/master/config/jobs/image-pushing/k8s-staging-dranet.yaml) in kubernetes/test-infra, which runs a Cloud Build job using [cloudbuild.yaml](./cloudbuild.yaml).

## 2. Generate release notes

Use the [release-notes tool](https://github.com/kubernetes/release/blob/master/cmd/release-notes/README.md) to generate release notes from PR descriptions since the previous tag:

```bash
export GITHUB_TOKEN=$(gh auth token)
release-notes \
  --org kubernetes-sigs \
  --repo dranet \
  --branch main \
  --start-rev <Previous revision, e.g. v1.1.0> \
  --skip-first-commit \
  --dependencies=false \
  --end-sha <Commit SHA, e.g. 81bd6fe8fb1f498ab900b33dbda3ae1db6771cd8> \
  --output release-notes.md
```
Review and edit `release-notes.md`.

## 3. Cloud Build produces staging artifacts

Cloud Build runs `make ensure-helm release`, which builds and pushes the container image and the Helm chart to the staging registries:
- `gcr.io/k8s-staging-networking/dranet:v1.1.0`
- `gcr.io/k8s-staging-networking/charts/dranet:1.1.0`

> **Note:** Helm chart versions follow semver without the `v` prefix (e.g. `1.1.0`),
> while container image tags use the `v` prefix (e.g. `v1.1.0`).

## 4. Retrieve artifact digests

Once the Cloud Build run succeeds, retrieve the SHA digests for both artifacts:
```bash
crane digest gcr.io/k8s-staging-networking/dranet:v1.1.0
crane digest gcr.io/k8s-staging-networking/charts/dranet:1.1.0
```

## 5. Promote artifacts to production

Open a PR against [k8s.io](https://github.com/kubernetes/k8s.io) to add both
artifacts to [registry.k8s.io/images/k8s-staging-networking/images.yaml](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/images/k8s-staging-networking/images.yaml):

```yaml
- name: dranet
  dmap:
    "sha256:<IMAGE_DIGEST>": ["v1.1.0"]

- name: charts/dranet
  dmap:
    "sha256:<CHART_DIGEST>": ["1.1.0"]
```

Once the PR is merged, the [`post-k8sio-image-promo`](https://testgrid.k8s.io/sig-k8s-infra-k8sio#post-k8sio-image-promo) postsubmit job in kubernetes/test-infra will promote both artifacts from `gcr.io/k8s-staging-networking` to `registry.k8s.io/networking`.

## 6. Verify the promotion

```bash
# Verify the container image
crane digest registry.k8s.io/networking/dranet:v1.1.0

# Verify the Helm chart artifact
crane digest registry.k8s.io/networking/charts/dranet:1.1.0
# or
helm show chart oci://registry.k8s.io/networking/charts/dranet --version 1.1.0
```

## 7. Publish the GitHub Release

Publish the release notes prepared in step 1 as a [GitHub Release](https://github.com/kubernetes-sigs/dranet/releases/new) against the tag. At this point all artifacts are available at `registry.k8s.io` and the release is safe to announce in [#wg-device-management](https://kubernetes.slack.com/archives/C0409NGC1TK) and  [sig-network](https://kubernetes.slack.com/archives/C09QYUH5W) Slack groups.
