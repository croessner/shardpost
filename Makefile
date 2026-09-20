IMAGE      ?= docker.io/chrroessner/shardpost
TOOLS      ?= docker.io/chrroessner/shardpost-tools
TAG        ?= 0.24.0
TOOLS_TAG  ?= 0.13.0
CONTEXT    ?= croessner
OVERLAY    ?= deploy/overlays/croessner-nauthilus

GOLANGCI_LINT_VERSION := 2.13.2
OAPI_CODEGEN          := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
ADMIN_API_SPEC        := api/admin/openapi.yaml
ADMIN_API_CONFIG      := api/admin/oapi-codegen.yaml
ADMIN_API_GEN         := internal/adminapi/api.gen.go
LINT_IMAGE ?= shardpost-lint:$(GOLANGCI_LINT_VERSION)


.PHONY: build build-tools push deploy undeploy status lint lint-image lint-local test test-docker test-programs test-programs-ldap contrib-ucm-renew-test contrib-ucm-renew-lint contrib-ucm-renew-build test-static-cluster logs generate check-generate man

build:
	docker build --platform linux/amd64 --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) .

build-tools:
	docker build --platform linux/amd64 -f Dockerfile.tools -t $(TOOLS):$(TOOLS_TAG) .

push:
	docker push $(IMAGE):$(TAG)
	docker push $(TOOLS):$(TOOLS_TAG)

deploy:
	kubectl --context $(CONTEXT) apply -k $(OVERLAY)

undeploy:
	kubectl --context $(CONTEXT) delete -k $(OVERLAY)

status:
	kubectl --context $(CONTEXT) -n rook-ceph get cephblockpool shardpost-index shardpost-data cephclient shardpost
	kubectl --context $(CONTEXT) -n shardpost get pods,svc

logs:
	kubectl --context $(CONTEXT) -n shardpost logs -l app.kubernetes.io/name=shardpost-server --tail=100 -f

# The admin API code is generated from the OpenAPI contract; the
# generator runs outside the vendor tree (module mode, needs the network).
generate:
	GOFLAGS=-mod=mod $(OAPI_CODEGEN) -config $(ADMIN_API_CONFIG) $(ADMIN_API_SPEC)

# Fails when the committed generated code does not match the contract.
check-generate:
	@tmp=$$(mktemp -d) && sed "s#^output:.*#output: $$tmp/api.gen.go#" $(ADMIN_API_CONFIG) > $$tmp/config.yaml \
		&& GOFLAGS=-mod=mod $(OAPI_CODEGEN) -config $$tmp/config.yaml $(ADMIN_API_SPEC) \
		&& cmp -s $$tmp/api.gen.go $(ADMIN_API_GEN); rc=$$?; rm -rf $$tmp; \
		if [ $$rc -ne 0 ]; then echo "$(ADMIN_API_GEN) is stale, run make generate"; exit 1; fi

# man renders every manual page and fails on a formatting error. Needs
# mandoc or groff; on macOS man(1) is mandoc.
man:
	@for f in docs/man/man1/*.1; do \
		out=$$(man ./$$f 2>&1 >/dev/null) || exit 1; \
		if [ -n "$$out" ]; then echo "$$f: $$out"; exit 1; fi; \
	done; \
	echo "manual pages ok"

lint: lint-image
	docker run --rm --platform linux/amd64 --user $$(id -u):$$(id -g) \
		-v $(CURDIR):/src -v shardpost-lint-cache:/cache -w /src \
		$(LINT_IMAGE) run --build-tags tentacle ./...

# The lint image carries librados-devel (the cgo packages need it to
# type-check) and the pinned golangci-lint; rebuilt only when missing.
lint-image:
	@docker image inspect $(LINT_IMAGE) >/dev/null 2>&1 || \
		docker build --platform linux/amd64 -f Dockerfile.lint \
			--build-arg GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) -t $(LINT_IMAGE) .

# Host variant for Linux machines that have librados-devel installed.
lint-local:
	@golangci-lint version | grep -q 'version $(GOLANGCI_LINT_VERSION) ' || (echo 'wrong golangci-lint version'; exit 1)
	golangci-lint run --build-tags tentacle ./...

test:
	go test -race ./internal/config ./internal/logging ./internal/auth ./internal/imapproto ./internal/pop3proto ./internal/sieve ./internal/imapsieve ./internal/extprograms ./internal/delivery ./internal/admin ./cmd/shardpostctl ./internal/migrate ./internal/memstore ./internal/quota ./internal/provision ./internal/fts ./internal/postings ./internal/telemetry ./internal/prototrace ./internal/inflate ./internal/jobs ./internal/archive

# Python programs, in the runtime base image with the same venv as the
# image (programs/requirements.txt, hash-pinned).
test-programs:
	docker run --rm --platform linux/amd64 -e PYTHONDONTWRITEBYTECODE=1 -v $(CURDIR)/programs:/programs:ro \
		quay.io/centos/centos:stream9 sh -c 'dnf -q -y install python3 >/dev/null && python3 -m venv /tmp/venv \
		&& /tmp/venv/bin/pip -q --disable-pip-version-check install --require-hashes --no-deps --only-binary=:all: -r /programs/requirements.txt \
		&& cd /programs && /tmp/venv/bin/python -m unittest discover -p "test_*.py"'

# quota_flag.py against a throwaway OpenLDAP (mailstack image, RNS schema,
# SASL EXTERNAL, the proposed provider ACL).
test-programs-ldap:
	./hack/quota-flag-ldap-test.sh

# contrib/ucm-cert-renew: separate Go module and image (not part of ShardPost).
UCM_RENEW_DIR   := contrib/ucm-cert-renew
UCM_RENEW_IMAGE ?= docker.roessner-net.de/mail/ucm-cert-renew
UCM_RENEW_TAG   ?= 0.1.1

contrib-ucm-renew-test:
	cd $(UCM_RENEW_DIR) && go test -race ./...

contrib-ucm-renew-lint: lint-image
	docker run --rm --platform linux/amd64 --user $$(id -u):$$(id -g) \
		-v $(CURDIR):/src -v shardpost-lint-cache:/cache -w /src/$(UCM_RENEW_DIR) \
		$(LINT_IMAGE) run --config /src/.golangci.yml ./...

contrib-ucm-renew-build:
	docker build --platform linux/amd64 -t $(UCM_RENEW_IMAGE):$(UCM_RENEW_TAG) $(UCM_RENEW_DIR)

# Writes test mail; only for an explicitly selected isolated static-auth cluster.
test-static-cluster:
	CONTEXT=$(CONTEXT) hack/test.sh

# Full test suite including the librados packages, in the lint image.
test-docker: lint-image
	docker run --rm --platform linux/amd64 --user $$(id -u):$$(id -g) \
		-v $(CURDIR):/src -v shardpost-lint-cache:/cache -w /src --entrypoint go \
		$(LINT_IMAGE) test -race -tags tentacle ./...
