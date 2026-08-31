# Quickstart

Ten minutes, one kind cluster, no cloud account. You will declare an artifact
(an OCI image in a throwaway in-cluster registry), watch the controller build
it, then destroy the artifact behind the controller's back and watch it come
back. That last part is the point of the project.

Prerequisites: `kind`, `kubectl`, `helm`, and this repo:

```sh
git clone https://github.com/kargops/artifact-controller
cd artifact-controller
```

## 1. A cluster and a disposable registry

```sh
kind create cluster --name artifacts-demo

kubectl create namespace quickstart
kubectl -n quickstart create deployment registry --image=registry:3 --port=5000
kubectl -n quickstart expose deployment registry --port=5000
```

No containerd or registry wiring is needed: nothing ever *pulls* the demo
image. The controller only checks manifests over HTTP, and the generator only
pushes. The registry's storage is deliberately ephemeral — restarting the pod
wipes it, which is exactly what step 5 exploits.

## 2. The controller

```sh
helm install artifact-controller charts/artifact-controller \
  -n artifact-system --create-namespace
```

The controller *image* is pulled from GHCR. If the package isn't public yet
(or you're on a fork), build it into the cluster instead:

```sh
make docker-build VERSION=dev
kind load docker-image ghcr.io/kargops/artifact-controller:dev --name artifacts-demo
helm upgrade artifact-controller charts/artifact-controller \
  -n artifact-system --set image.tag=dev
```

The chart's defaults already allow `batch/v1` Jobs as generators (an
aggregation-labelled ClusterRole; other engines are values toggles).

## 3. An ArtifactClass: where artifacts live, and how to make one

```yaml
# class.yaml
apiVersion: artifacts.kargops.dev/v1alpha1
kind: ArtifactClass
metadata:
  name: quickstart
spec:
  store:
    driver: oci
    oci:
      repository: registry.quickstart.svc.cluster.local:5000/demo
      insecure: true          # plain-HTTP registry
  generator:
    # Any Kubernetes object works as a generator. Here: a Job that "builds"
    # by copying a small public image to the content-addressed tag. The
    # crane image's entrypoint is crane, so only args are set.
    template:
      apiVersion: batch/v1
      kind: Job
      spec:
        backoffLimit: 0       # retries belong to the controller
        template:
          spec:
            restartPolicy: Never
            containers:
              - name: generate
                image: gcr.io/go-containerregistry/crane:v0.20.3
                args:
                  - copy
                  - registry.k8s.io/pause:3.10
                  - registry.quickstart.svc.cluster.local:5000/demo:{{ .Key }}
                  - --insecure
    succeededWhen: "status.conditions.exists(c, c.type == 'Complete' && c.status == 'True')"
    failedWhen: "status.conditions.exists(c, c.type == 'Failed' && c.status == 'True')"
    inProgressWhen: "has(status.active) && status.active > 0"
    progressDeadline: 5m
  backoff: { maxAttempts: 3, initialDelay: 30s, maxDelay: 5m }
  verificationGracePeriod: 1m
```

```sh
kubectl apply -f class.yaml
```

## 4. An Artifact: the declaration

```yaml
# artifact.yaml
apiVersion: artifacts.kargops.dev/v1alpha1
kind: Artifact
metadata:
  name: hello
  namespace: quickstart
spec:
  classRef: { name: quickstart }
  identity:
    demo: hello               # hashed into the store key (the image tag)
  interval: 30s               # re-verification cadence
```

```sh
kubectl apply -f artifact.yaml
kubectl -n quickstart get artifacts -w
```

Expect, within a minute:

```
NAME    STATE        AGE
hello   Generating   2s      # a Job named hello-<hash>-r1 appears
hello   Ready        40s     # manifest observed in the registry
```

`kubectl -n quickstart describe artifact hello` shows the conditions, the
content-addressed key, and the digest the store reported.

## 5. Destroy the artifact — the actual demo

```sh
kubectl -n quickstart rollout restart deployment registry
kubectl -n quickstart get artifacts -w
```

The restart wipes the registry's storage: the artifact is gone and nobody told
the controller. Within one `interval` it notices, runs a fresh Job (`…-r2`),
and returns to `Ready` — no pipeline rerun, no human.

Two things worth noticing while it happens:

- **The digest comes back identical.** The generator copies an immutable
  upstream image, so the rebuilt bytes are the same. A real build (Dockerfile,
  compiler) is *not* reproducible — which is why verification uses a
  provenance stamp, not the digest, and why the digest exists for drift
  detection rather than identity.
- **No stamp was written**, and the controller adopted the artifact anyway: an
  *absent* stamp is adoptable; only a *foreign* stamp is a `KeyConflict`. For
  real classes, have the generator stamp the manifest
  (`dev.kargops.artifacts.spec-hash` — the class template exposes
  `{{ .SpecHash }}` for exactly this).

## 6. Clean up

```sh
kind delete cluster --name artifacts-demo
```

## Where to go next

- `config/samples/` — real classes: S3 + Tekton, S3 + Argo Workflows, ECR
  images built by a Tekton kaniko pipeline, an HTTP-driver class for Nexus,
  and an Entra ID account observed via Microsoft Graph (read its header
  warning first).
- README "Store drivers" and "Lifecycle semantics" — what `ttl`,
  `deleteAfter`, `deletionPolicy`, drift policies and failure budgets do.
