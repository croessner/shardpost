# syntax=docker/dockerfile:1
#
# Build stage: the Ceph 20 (Tentacle) image plus librados-devel from the
# CentOS Storage SIG, so the binary links against the same librados version
# that runs in the cluster (aes256k cephx keys need a Tentacle client).
FROM quay.io/ceph/ceph:v20.2.4-20260818 AS build
RUN dnf -y install centos-release-ceph-tentacle \
 && dnf -y install librados-devel gcc \
 && dnf clean all
COPY --from=golang:1.27 /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH CGO_ENABLED=1 GOFLAGS=-mod=vendor
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal
COPY programs ./programs
RUN go vet -tags tentacle ./... \
 && LDFLAGS="-s -w -X shardpost/internal/version.Version=${VERSION}" \
 && go build -tags tentacle -trimpath -ldflags="$LDFLAGS" -o /out/shardpost ./cmd/shardpost \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/shardpostctl ./cmd/shardpostctl \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shardpost-load ./cmd/shardpost-load

# Python dependencies of the external programs (quota_flag.py needs an
# LDAP client): a venv built on the runtime base, hash-pinned wheels only.
FROM quay.io/centos/centos:stream9 AS venv
RUN dnf -y install python3 && dnf clean all
COPY programs/requirements.txt /tmp/requirements.txt
RUN python3 -m venv /usr/local/lib/shardpost/venv \
 && /usr/local/lib/shardpost/venv/bin/pip install --no-cache-dir --disable-pip-version-check \
      --require-hashes --no-deps --only-binary=:all: -r /tmp/requirements.txt

# Runtime stage: minimal CentOS Stream 9 with librados2 and python3 for
# the external programs (learn.py and quota_warning.py use the standard
# library only, quota_flag.py the venv).
FROM quay.io/centos/centos:stream9
LABEL org.opencontainers.image.title="ShardPost" \
      org.opencontainers.image.description="Distributed mail server backed by Ceph RADOS" \
      org.opencontainers.image.version="0.26.1"
RUN dnf -y install epel-release centos-release-ceph-tentacle \
 && dnf -y install librados2 python3 \
 && dnf clean all && rm -rf /var/cache/dnf \
 && groupadd -g 167 ceph && useradd -u 167 -g 167 -M -s /sbin/nologin ceph
COPY --from=build /out/shardpost /out/shardpostctl /out/shardpost-load /usr/local/bin/
# External programs: only entries in extprograms.programs can run them,
# never through a shell; root-owned and not group/world-writable.
COPY --from=venv /usr/local/lib/shardpost/venv /usr/local/lib/shardpost/venv
COPY --chmod=0755 programs/learn.py programs/quota_warning.py programs/quota_flag.py /usr/local/lib/shardpost/programs/
COPY programs/templates/ /usr/local/lib/shardpost/programs/templates/
USER 167:167
EXPOSE 1143 2424 8080
ENTRYPOINT ["/usr/local/bin/shardpost"]
