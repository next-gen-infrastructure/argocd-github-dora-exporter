FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /argocd-github-dora-exporter ./cmd/argocd-github-dora-exporter

FROM gcr.io/distroless/static-debian12
COPY --from=build /argocd-github-dora-exporter /argocd-github-dora-exporter
USER nonroot:nonroot
ENTRYPOINT ["/argocd-github-dora-exporter"]
