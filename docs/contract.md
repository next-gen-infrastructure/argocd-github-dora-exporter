# argocd-github-dora-exporter contract

Prometheus exporter that derives DORA metrics and delivery-pipeline timings from
**GitHub** (pull requests, checks, workflow runs) and **Argo CD** (what was deployed, when).
This document is the interface between the exporter, its deployment module and its dashboard.
Nothing here is specific to one organisation: all names come from configuration.

## Model

One exporter instance per cluster/environment. It watches that cluster's Argo CD
`Application`s and reads GitHub for the repositories those applications deploy.

```
commit -> PR opened -> ready for review -> PR merged -> package built -> Argo CD synced
 coding      draft           review          package_queue/build        rollout
```

Lead time is **per commit**: `deployedAt - commit time`, observed when the commit first
reaches the environment.

## Configuration

Environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `GITHUB_APP_ID` | required unless a token is set | GitHub App id |
| `GITHUB_APP_INSTALLATION_ID` | required | installation id |
| `GITHUB_APP_PRIVATE_KEY` | required | PEM (or `GITHUB_APP_PRIVATE_KEY_FILE`) |
| `GITHUB_TOKEN` / `GITHUB_TOKEN_FILE` | none | static token (e.g. a read-only fine-grained PAT) used instead of the App credentials; for local runs and tests |
| `APP_FILTER` | none | regex on the Application name; only matching Applications are tracked (staged rollout, tests) |
| `INITIAL_LOOKBACK` | `0` | how far back the first PR and build poll looks (e.g. `24h`); only for a first deployment or tests, since a restart would count those PRs and runs again |
| `GITHUB_API_URL` | `https://api.github.com` | API root, for GitHub Enterprise Server |
| `ENVIRONMENT` | required | value of the `environment` label (`dev`, `prod`, ...) |
| `ARGOCD_NAMESPACE` | `argocd` | namespace holding `Application`s |
| `POLL_INTERVAL` | `5m` | GitHub polling period. Argo CD is **watched**, not polled |
| `ANNOTATION_PREFIX` | `dora-exporter` | prefix of the per-app annotations, below |
| `DEFAULT_BRANCH` | required | base branch release PRs target in this environment |
| `TAG_REGEX` | `^(?:dev\|sha)-(?P<ref>[0-9a-f]{7,40})` | one named group: `ref` |
| `TAG_RESOLVER` | `sha` | only `sha` is implemented; anything else is a startup error |
| `PACKAGE_WORKFLOW` | empty | last-resort package workflow file, used only when an app has no `package-workflow` annotation and the workflow cannot be discovered (see "Repository and workflow resolution"); empty means build, rollout and image update timings are skipped for such apps |
| `CHECKS_INCLUDE` | `.*` | regex limiting the `check` label (cardinality) |
| `FIRST_REVIEW` | `false` | enables `dora_pr_first_review_seconds` |
| `LISTEN_ADDR` | `:9090` | serves `/metrics` and `/healthz` |

There is no central list of repositories or apps. Per-environment settings above apply to every
app; per-app facts come from the Application itself (next section).

`TAG_RESOLVER=sha` treats `ref` as a (possibly short) commit sha. Anything after the captured
group (for example a second `-<sha>` suffix) is ignored. A `semver` resolver (tag -> commit
through the tags API) is a planned extension; the regex/resolver split exists so adding it does
not change this contract.

## Selecting applications

An Application is tracked when either

1. an **Argo CD Image Updater** `ImageUpdater` CR in `ARGOCD_NAMESPACE` manages it, or
2. it has the annotation `<prefix>/enabled: "true"`.

`<prefix>/enabled: "false"` opts an Application out. With the first rule no annotation is needed.
All annotations are optional overrides, with `<prefix>` = `ANNOTATION_PREFIX`.

**Image Updater CRs** (`argocd-image-updater.argoproj.io/v1alpha1`, read-only, optional): an
Application matches when its name matches an `spec.applicationRefs[].namePattern` (glob). The
matched `images[]` entry gives the tracked image (`imageName`) and its `alias`. Pick by image name
if the CR lists several. `spec.writeBackConfig.gitConfig` (`writeBackTarget`, `branch`) says where image updater writes the
new tag. (`status.recentUpdates[].updatedAt` is not used: it is a status-update time that lags the
real write-back by minutes, and the list is short.)
If the CRD does not exist or is not readable, only annotated Applications are tracked. The CRD is
`v1alpha1`: the annotations stay as a stable fallback.

The tracked image matters because an Application can run several images (a sidecar or an
instrumentation image can be listed first in `status.summary.images`).

| Annotation | Meaning | Default |
|---|---|---|
| `<prefix>/enabled` | `"true"` opts in, `"false"` opts out | tracked only if an ImageUpdater CR matches |
| `<prefix>/image` | which image in `status.summary.images` is the tracked one | the ImageUpdater image, else the first one |
| `<prefix>/repo` | GitHub `owner/name` | the repository the image's GHCR package is linked to, else derived from the image path |
| `<prefix>/package-workflow` | workflow file that builds the image | discovered from the application's image, else `PACKAGE_WORKFLOW` |
| `<prefix>/tag-regex` | overrides `TAG_REGEX` | `TAG_REGEX` |
| `<prefix>/default-branch` | overrides `DEFAULT_BRANCH` | `DEFAULT_BRANCH` |

**Repo derivation:** the repository is `owner/name` of the image `[registry/]owner/name[:tag]`
without the registry host (the first segment when it contains `.` or `:`), unless it is resolved
or annotated (next section). The metrics `repo` label is `name` only.

## Repository and workflow resolution

An image is not always built in a repository of the same name (a package `owner/concierge` may come
from `owner/concierge-ai`), and repositories name their package workflow differently
(`ci.yml`, `ci-dev.yaml` in dev and `ci-main.yaml` in prod, ...). So neither is assumed.

**Repository.** Precedence: the `repo` annotation, then the repository the GHCR container package is
linked to (`GET /orgs/{owner}/packages/container/{name}`, falling back to `/users/{owner}/...`,
field `repository.full_name`), then the path-derived name. The result is cached (6 h; a failed or
empty lookup for 1 h) and a failure is logged once per image, so a missing permission falls back
quietly to the derived name. The lookup runs when an Application is first seen and blocks only on a
cache miss.

**Package workflow.** Precedence: the `package-workflow` annotation, then the discovered workflow,
then `PACKAGE_WORKFLOW`. Discovery uses the application's current image: take the commit of the
tag, list its workflow runs (`GET /repos/{owner}/{repo}/actions/runs?head_sha=`) and pick the run
that built the image, that is the run which

* was not triggered by a pull request,
* succeeded,
* started before the image updater write-back commit (the commit that set the tag, found as for
  `dora_image_update_seconds`), and
* finished no more than 5 minutes before it (image updater polls the registry).

If several runs qualify, the one that finished closest to the write-back wins. Its workflow file
(the base name of the run's `path`) is cached for the repository (6 h; a failed discovery 1 h). It is
discovered at start-up from the live tag and again at every deployment, so a workflow that is
renamed, or that differs per environment, is followed without configuration. Applications without a
known write-back file (no ImageUpdater CR, or a non-helmvalues target) cannot be discovered and use
the annotation or `PACKAGE_WORKFLOW`.

Because annotations are read from the Application, they can be set once as constants in an
ApplicationSet template (`enabled`, `package-workflow`) and need no per-app file.

## Behaviour

### Deployments (Argo CD)

- Watch `applications.argoproj.io` in `ARGOCD_NAMESPACE` (list + watch, resync on error).
- Only tracked Applications are used (see "Selecting applications"). Find the tracked image in
  `status.summary.images` and extract `ref` from its tag with the effective tag regex.
  `status.summary.images` is built from the live resource tree (pods included), so during a rolling
  update the old and the new tag of the same image are both listed. While more than one distinct
  tag is listed, the live tag is unknown: the Application has no ref and nothing is derived from
  it until one tag remains.
- Every entry of `status.history` is a deployment, keyed `(app, deployedAt)`. Deduplicate on
  that key. Argo CD may keep only a few history entries, so react to watch events, do not rely
  on history alone.
- **Start-up:** record existing history as already seen. Do not emit events for it.
- `result`: `success` when `status.operationState.phase` is `Succeeded`; `failure` when it is
  `Failed` or `Error`. A sync that succeeded but is observed `Degraded` afterwards also counts as
  `failure` (at most one failure per sync). "Afterwards" means within 30 minutes of the sync's
  `deployedAt`; a later Degraded state is not blamed on that sync.
- A failed sync never appears in `status.history`; it is detected from
  `status.operationState` (`phase` and `finishedAt`).
- **Restore time:** from the first `failure` of an app until its next sync that ends `Healthy`.
  Observed when the healthy sync is seen.

### Lead time and commits

- A deployment for lead time is a **change of the tracked image ref** of an app (`status.summary.images`
  is refreshed after the sync, so the newest history entry already belongs to the new ref). With
  new ref `R`, previous ref `P` of the same app, and `deployedAt` of the newest history entry
  (now if absent), call the GitHub compare API `P...R` (paginate) and observe one
  `dora_lead_time_seconds` per commit: `deployedAt - commit.committer.date` (0 if negative).
- This work runs off the watch goroutine in a queue of 64; a full queue drops the deployment and
  logs a warning.
- Observe each `(repo, environment, commit sha)` **once**, the first time it is deployed, even if
  several apps ship the same repo.
- Skip merge commits (more than one parent). Skip the first deployment seen for an app (no `P`).
- Compare is read 100 commits per page and capped at 1000 commits per deployment.

### Pull requests (GitHub GraphQL)

Every `POLL_INTERVAL`, for each tracked repository (derived from tracked Applications), read the
50 most recently updated PRs merged into the repository's default branch. One query returns for
each PR: `createdAt`, `mergedAt`, the author, the first commit, the dates of the last 100 commits,
the last commit with its `statusCheckRollup` contexts, `timelineItems(itemTypes:
[READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT])`, and the first 10 reviews.

A repository that GitHub reports as not found (wrong image to repository mapping, or not visible
to the App) is skipped and logged once; it does not fail the poll for the other repositories.

Only PRs **merged after the exporter started** are observed, each once. Nothing is backfilled:
earlier PRs were counted by a previous process or predate the exporter. PRs merged while the
exporter is down are lost (the same rule as for deployments).

Stages (`dora_pr_stage_seconds{stage}`):

| `stage` | Interval |
|---|---|
| `coding` | first commit date -> `createdAt` (0 if negative) |
| `draft` | `createdAt` -> review start |
| `review` | review start -> `mergedAt` |

**Review start** = the last `READY_FOR_REVIEW_EVENT` before the merge; if there is none,
`createdAt`. A PR converted back to draft and re-readied therefore counts the later ready time,
and the time in between is in `draft`.

### Checks on the final head commit (all checks, no required-only filtering)

- `dora_pr_checks_seconds`: earliest check `startedAt` -> latest `completedAt`.
- `dora_pr_check_queue_seconds`: head commit `committedDate` -> earliest `startedAt`
  (approximation: a push can be later than the commit date).
- `dora_pr_check_duration_seconds{check,conclusion}` and `dora_pr_check_runs_total`: per check
  run. `check` is restricted by `checks_include`; names not matching become `other`.
- `dora_pr_merge_delay_seconds`: latest check `completedAt` -> `mergedAt` (0 if negative).
- `dora_pr_head_commits`: commits made after the review start (the head commit counts once), at
  least 1. This approximates the number of pushes after the PR became reviewable; the commit
  list does not expose push events.
- Legacy commit statuses (`StatusContext`) have no start time: counted, not timed.
- `dora_pr_first_review_seconds` (only when `first_review: true`): review start -> first review
  or comment by someone other than the author.

### Builds (only when `package_workflow` is set)

Every `POLL_INTERVAL`, read the 50 newest completed runs of that workflow created after the
exporter started, and for each run not seen before its jobs (`/actions/runs/{id}/jobs`). Skipped
jobs and jobs without start or completion times are ignored. Runs triggered by a pull request
(`event` is `pull_request` or `pull_request_target`) are ignored: the same workflow often builds
without pushing on PRs, and those runs are not package builds. If GitHub answers 404 for the
workflow (it does not exist in that repository), there is no build data for it: this is logged
once per repository and is not a failed poll.

- `dora_build_queue_seconds`: job `created_at` -> `started_at` (runner wait, cold start).
- `dora_build_duration_seconds`: job `started_at` -> `completed_at`.
- `dora_rollout_seconds`: for the new ref of a deployment, the package run for the head commit of
  the compare range -> `deployedAt`. The run is the **earliest successful** run of the package
  workflow for that commit (it pushed the image; later re-dispatches or re-runs finish after the
  deployment and are ignored), and its `updated_at` is the finish time. Includes image
  updater's write-back delay and, where syncs are manual, the wait for the release window.
- `dora_image_update_seconds`: the same package run (`updated_at` as above) -> the time image updater wrote the new tag
  to the GitOps repository. The GitOps repository, branch and path come from the Application's
  `spec.source`; the values file is the CR's `writeBackTarget` (`helmvalues:./values-dev.yaml` ->
  `<source.path>/values-dev.yaml`; other targets are not understood and skip the metric). The
  write-back is the newest commit on the branch touching that file whose message contains
  `to '<tag>'` (image updater's own "updates image X tag 'old' to 'new'"), so a later commit that
  mentions the tag as the old one is not matched. `rollout - image_update` is the part after the
  write-back (sync, or waiting for a manual sync). This needs read access (Contents) to the GitOps
  repository.

Stages sum to lead time for single-commit deployments; for batch deployments (a release) they are
approximate. That is accepted.

## Metrics

All carry `repo` and `environment` unless noted. Time unit: seconds.

| Metric | Type | Extra labels |
|---|---|---|
| `dora_deployments_total` | counter | `app`, `result` (`success`\|`failure`) |
| `dora_last_deployment_timestamp_seconds` | gauge | `app` |
| `dora_lead_time_seconds` | histogram | |
| `dora_restore_time_seconds` | histogram | `app` |
| `dora_pr_stage_seconds` | histogram | `base`, `stage` (`coding`\|`draft`\|`review`) |
| `dora_pr_first_review_seconds` | histogram | `base` |
| `dora_pr_checks_seconds` | histogram | `base` |
| `dora_pr_check_queue_seconds` | histogram | `base` |
| `dora_pr_check_duration_seconds` | histogram | `base`, `check`, `conclusion` |
| `dora_pr_check_runs_total` | counter | `base`, `check`, `conclusion` |
| `dora_pr_merge_delay_seconds` | histogram | `base` |
| `dora_pr_head_commits` | histogram | `base` |
| `dora_build_queue_seconds` | histogram | `workflow`, `conclusion` |
| `dora_build_duration_seconds` | histogram | `workflow`, `conclusion` |
| `dora_rollout_seconds` | histogram | `app` |
| `dora_image_update_seconds` | histogram | `app` |
| `dora_exporter_github_requests_total` | counter | `api` (`rest`\|`graphql`), `code` |
| `dora_exporter_github_rate_limit_remaining` | gauge | `api` |
| `dora_exporter_last_poll_timestamp_seconds` | gauge | `source` (`github`\|`argocd`) |

(`repo` and `environment` are omitted on the three `dora_exporter_*` metrics except where noted:
`repo` is absent on the two rate-limit and poll metrics.)

Buckets (seconds), explicit, since defaults are useless here:

- Lead time, restore time and PR stages: `60, 300, 900, 1800, 3600, 7200, 14400, 28800, 86400,
  172800, 345600, 604800, 1209600, 2592000`.
- Checks, builds, queue times, rollout, merge delay: `5, 15, 30, 60, 120, 300, 600, 900, 1800,
  3600, 7200`.
- `dora_pr_head_commits`: `1, 2, 3, 5, 8, 13, 21`.

Counters and histograms restart at zero with the process; that is fine for `increase()`.

## Kubernetes access

Read-only: `get`, `list`, `watch` on `applications.argoproj.io` and, optionally,
`imageupdaters.argocd-image-updater.argoproj.io` in `ARGOCD_NAMESPACE`. Nothing else.
The Helm chart (`charts/argocd-github-dora-exporter`) creates the Role, RoleBinding,
ServiceAccount, Service (port named `metrics`) and an optional PodMonitor. Run a single replica:
deduplication state is in memory.

## GitHub App permissions

Read-only, on the application repositories and on the GitOps repository:

* Repository: Metadata, Contents, Pull requests, Actions, **Checks** and **Commit statuses** (the
  check data of the PR timings is read through GraphQL `statusCheckRollup`).
* Organisation: **Packages** (finds the repository of an image). Optional: without it the
  repository is derived from the image path or taken from the annotation.

## Assumptions to verify

1. **Release PRs must keep their commits** (merge commit, not squash). With a squash merge the
   compare range is a single commit and per-commit lead time collapses to one data point per
   release.
2. `status.summary.images` contains the deployed image tag and one Application maps to one repo.
3. The image's GHCR package is linked to its source repository (or the `repo` annotation is set); without
   either, the image path is assumed to equal the GitHub `owner/name`.
4. Short shas are unique enough to resolve inside the tracked repository.
5. The package workflow builds on push or dispatch. It may also run on pull requests (building
   without pushing); those runs are ignored. Runs are not filtered by branch, because a `repository_dispatch` run reports the repository's default
   branch even when it builds for another environment. Cancelled runs are kept under
   `conclusion="cancelled"`; use `conclusion="success"` for build duration panels.
