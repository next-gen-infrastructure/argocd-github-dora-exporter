# argocd-github-dora-exporter

Prometheus exporter that derives [DORA](https://dora.dev) metrics and delivery-pipeline timings
from **Argo CD** (what was deployed, when) and **GitHub** (pull requests, checks, workflow runs).

It watches Argo CD `Application`s, reads GitHub through a read-only GitHub App, and exposes
metrics on `/metrics`. Applications opt in with an annotation; there is no central list of
repositories.

- Behaviour, configuration, annotations and the full metric list: [docs/contract.md](docs/contract.md)
- Helm chart: [charts/argocd-github-dora-exporter](charts/argocd-github-dora-exporter)
  (published to `oci://ghcr.io/next-gen-infrastructure/charts`)
- Image: `ghcr.io/next-gen-infrastructure/argocd-github-dora-exporter`

## Dashboard

The chart can ship a Grafana dashboard as a [grafana-operator](https://github.com/grafana/grafana-operator)
v5 `GrafanaDashboard`:

```yaml
grafanaDashboard:
  enabled: true
  instanceSelector:
    matchLabels: {instance: grafana}   # labels of your Grafana resource
  allowCrossNamespaceImport: true      # if Grafana runs in another namespace
  folderUID: dora                      # optional
```

It has a `datasource` variable, so any Prometheus-compatible data source works. The JSON is
`charts/argocd-github-dora-exporter/dashboards/dora.json`.

## Development

```sh
make setup                                  # mise install (go, helm, pre-commit) + register git hooks
go build ./cmd/argocd-github-dora-exporter
go test ./...
golangci-lint run                           # also run by pre-commit
helm lint charts/argocd-github-dora-exporter
```

Commits follow [Conventional Commits](https://www.conventionalcommits.org/); versions and the
changelog are managed by release-please, so don't edit `CHANGELOG.md` or the manifest by hand.

## License

Apache 2.0, see [LICENSE](LICENSE).
