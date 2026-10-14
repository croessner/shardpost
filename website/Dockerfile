# syntax=docker/dockerfile:1
# Build context: the repository root (the manual is rendered from docs/ and
# ARCHITECTURE.md). Dockerfile.dockerignore next to this file selects what goes in;
# the root .dockerignore, which drops *.md, does not apply.
FROM python:3.13.13-alpine AS build
RUN apk add --no-cache mandoc
WORKDIR /repo
COPY website/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt
# The whole tree (minus Dockerfile.dockerignore): build.py checks that every
# file a document links to exists before it points the link at GitHub.
COPY . .
ENV SHARDPOST_REPO=/repo
RUN python3 website/build.py

FROM nginxinc/nginx-unprivileged:1.31.6-alpine
LABEL org.opencontainers.image.title="shardpost-website" \
      org.opencontainers.image.description="Static website and manual for ShardPost (shardpost.de)" \
      org.opencontainers.image.vendor="Rößner Network Solutions" \
      org.opencontainers.image.licenses="GPL-3.0-or-later"
COPY website/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /repo/website/dist /usr/share/nginx/html
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
