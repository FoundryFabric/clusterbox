# gha-runner-scale-set

Installs the **GitHub Actions Runner Controller (ARC)** in scale-set mode onto a
clusterbox-managed cluster. This is the modern (`AutoscalingRunnerSet`) ARC
path; the legacy `RunnerDeployment` / `HorizontalRunnerAutoscaler` mode is
intentionally **not** supported here.

## What this installs

This addon installs only the **controller** chart
(`oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller`,
pinned to `0.10.1`) into the `arc-systems` namespace, plus a Kubernetes Secret
named `controller-manager-gh-credentials` that holds GitHub credentials in the
schema the runner scale-set chart consumes.

It does **not** install any runner scale sets. Scale sets are per-repo or
per-org and need operator-supplied configuration (which repo, which labels,
min/max runner counts, container image, etc.). See
[Creating a first scale set](#creating-a-first-scale-set) below for a worked
example you can `kubectl apply` after the controller is healthy.

| Component                                | Provided by this addon | Provided by operator |
| ---------------------------------------- | :--------------------: | :------------------: |
| `arc-systems` Namespace                  | ✓                      |                      |
| `controller-manager-gh-credentials` Secret | ✓                    |                      |
| Controller Helm release                  | ✓                      |                      |
| `AutoscalingRunnerSet` (per-repo / per-org) |                     | ✓                    |

## 1. Create a GitHub App

We strongly recommend the GitHub App authentication path over a Personal
Access Token: App tokens are short-lived, scoped to install targets, and don't
tie your runners to a single human's account.

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App** in
   your GitHub org (or personal account, for a single-repo setup).
2. Set:
   - **Homepage URL**: anything (e.g. your cluster's URL).
   - **Webhook**: disable.
   - **Repository permissions**:
     - `Actions`: Read & write
     - `Administration`: Read & write
     - `Checks`: Read
     - `Metadata`: Read (auto-selected)
   - **Organization permissions** (only if running org-scoped runners):
     - `Self-hosted runners`: Read & write
3. Click **Create GitHub App**, then on the resulting page note the **App ID**.
4. Scroll to **Private keys** and click **Generate a private key**. This
   downloads a `.pem` file — keep it; you'll need its full contents in step 3.
5. In the left sidebar of the App settings, click **Install App**, install it
   into the org or repos that should host runners, then click into the
   installation and copy the **Installation ID** from the URL
   (`https://github.com/settings/installations/<INSTALLATION_ID>`).

## 2. Store credentials in clusterbox secrets

The installer resolves secrets by `(app, env, provider, region)` against the
configured secrets backend (see `internal/secrets`). Set these keys, scoped to
`app=gha-runner-scale-set` and the env/provider/region of the target cluster:

| Key                       | Required (App path) | Required (PAT path) | What goes in it                                |
| ------------------------- | :-----------------: | :-----------------: | ---------------------------------------------- |
| `GH_APP_ID`               | ✓                   |                     | GitHub App ID (number, e.g. `123456`)          |
| `GH_APP_INSTALLATION_ID`  | ✓                   |                     | Installation ID (number, e.g. `987654`)        |
| `GH_APP_PRIVATE_KEY`      | ✓                   |                     | Full PEM private key, including BEGIN/END lines |
| `GH_PAT_TOKEN`            |                     | ✓                   | Classic PAT (`ghp_...`) with `repo` + `workflow` scope, or fine-grained equivalent |

**Authentication rule**: either the three GitHub App keys are set together, or
`GH_PAT_TOKEN` is set. Don't mix. v1 of this addon documents this rule rather
than enforcing it in the installer; if you set neither, the controller will
install but no scale set will be able to authenticate when it references this
Secret.

The exact CLI/secret-store path depends on your secrets backend; for the
Pulumi-config backend (the default in clusterbox today) these end up at:

```
clusterbox:gha-runner-scale-set:<env>:<provider>:<region>:GH_APP_ID
clusterbox:gha-runner-scale-set:<env>:<provider>:<region>:GH_APP_INSTALLATION_ID
clusterbox:gha-runner-scale-set:<env>:<provider>:<region>:GH_APP_PRIVATE_KEY
clusterbox:gha-runner-scale-set:<env>:<provider>:<region>:GH_PAT_TOKEN
```

## 3. Install

```bash
clusterbox addon install gha-runner-scale-set --cluster <cluster-name>
```

The installer:

1. Looks up the cluster's env/provider/region from the registry.
2. Resolves the four secret keys above.
3. Renders `manifests/namespace.yaml`, `manifests/secret.yaml`, and
   `manifests/helmchart.yaml`, substituting `${...}` placeholders with the
   resolved secret values.
4. Applies the rendered manifests via `kubectl apply` against the cluster's
   kubeconfig.
5. Records a `kind=addon` deployment row in the registry.

> **Heads up**: the installer's `helmchart` strategy support is still landing.
> Until then, `clusterbox addon install` will refuse this addon with a clear
> error; in the meantime an operator can `kubectl apply -f` the rendered
> manifests directly. Tracked in the addon installer follow-up.

## 4. Verify

```bash
kubectl --kubeconfig <kubeconfig> get pods -n arc-systems
```

You should see the controller pod running, e.g.:

```
NAME                                READY   STATUS    RESTARTS   AGE
arc-gha-rs-controller-xxxxx-yyyy    1/1     Running   0          45s
```

Also verify the credentials Secret exists:

```bash
kubectl --kubeconfig <kubeconfig> get secret controller-manager-gh-credentials -n arc-systems
```

## 5. Creating a first scale set

Scale sets are deployed separately (`kubectl apply` or your existing GitOps
flow), pointing at the credentials Secret this addon shipped.

For a single-repo runner pool:

```yaml
# my-repo-runners.yaml
apiVersion: actions.github.com/v1alpha1
kind: AutoscalingRunnerSet
metadata:
  name: my-repo-runners
  namespace: arc-runners
spec:
  githubConfigUrl: https://github.com/my-org/my-repo
  githubConfigSecret: controller-manager-gh-credentials
  minRunners: 0
  maxRunners: 5
  runnerGroup: default
  template:
    spec:
      containers:
        - name: runner
          image: ghcr.io/actions/actions-runner:latest
          command: ["/home/runner/run.sh"]
```

```bash
kubectl create namespace arc-runners
# The credentials Secret only exists in arc-systems; copy it into arc-runners
# (or use a SecretGenerator / ExternalSecrets) before applying the scale set.
kubectl --namespace arc-runners apply -f my-repo-runners.yaml
```

In your repo's workflow, target the runners with:

```yaml
jobs:
  build:
    runs-on: my-repo-runners
    steps: [...]
```

For an org-wide pool, set `githubConfigUrl: https://github.com/my-org` instead
and adjust the App's permissions accordingly (see step 1).

## 6. Troubleshooting

| Symptom | Likely cause | Fix |
| ------- | ------------ | --- |
| Controller pod `CrashLoopBackOff` with `failed to refresh installation token` | App ID / Installation ID mismatch, or private key is for a different App | Re-check the IDs in step 1; regenerate the private key if it was rotated |
| Scale set events show `403` from GitHub API | App lacks required permissions (Actions, Administration, Self-hosted runners) | Edit the App's permissions in GitHub, then re-install on the target org/repo |
| Scale set events show `not found` for the Secret | Secret only exists in `arc-systems`; scale sets need it in their own namespace | Copy or replicate the Secret into the scale set's namespace |
| Controller installs but no runners ever come up | Workflow `runs-on:` label doesn't match the `AutoscalingRunnerSet` name | Use the runner-set name as the runner label in your workflow |
| `clusterbox addon install` errors with `strategy "helmchart" is not yet supported` | Installer helm-chart support hasn't landed yet | Apply the rendered manifests directly (`clusterbox addon render` once available, or hand-render and `kubectl apply -f`) |

For deeper debugging, the upstream docs are the source of truth:
<https://github.com/actions/actions-runner-controller/tree/master/docs>.
