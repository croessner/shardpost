# syntax=docker/dockerfile:1
# ucm-cert-renew: a static binary on an empty base; runs as non-root.
FROM golang:1.27 AS build
ENV CGO_ENABLED=0 GOFLAGS=-mod=vendor
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY main.go ./
COPY renew ./renew
RUN go vet ./... && go build -trimpath -ldflags="-s -w" -o /out/ucm-cert-renew .

FROM scratch
LABEL org.opencontainers.image.title="ucm-cert-renew" \
      org.opencontainers.image.description="Renews UCM LDAP client certificates in Kubernetes Secrets"
COPY --from=build /out/ucm-cert-renew /ucm-cert-renew
USER 65532:65532
ENTRYPOINT ["/ucm-cert-renew"]
