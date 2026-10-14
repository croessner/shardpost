REGISTRY ?= docker.roessner-net.de
IMAGE    ?= $(REGISTRY)/web/shardpost-website
TAG      := $(shell date -u +%Y%m%d-%H%M)-$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
PLATFORM ?= linux/amd64
REMOTE_DIR = /srv/docker/shardpost-website
MGMT     ?= mgmt.roessner-net.de
TARGET   ?= node1.roessner-net.de
ANSIBLE  = cd /etc/ansible && ./venv/bin/ansible -i inventories/rns/hosts.yml $(TARGET) -b
VENV     ?= .venv

.PHONY: build serve image run push deploy clean

$(VENV)/bin/python: requirements.txt
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install -q -r requirements.txt
	touch $@

build: $(VENV)/bin/python   ## Render the site into dist/ (needs mandoc)
	$(VENV)/bin/python build.py

serve: build      ## Preview at http://127.0.0.1:8000
	python3 -m http.server 8000 --bind 127.0.0.1 --directory dist

image:            ## Build the container image; the context is the repository root
	docker build --platform $(PLATFORM) -f Dockerfile -t $(IMAGE):$(TAG) -t $(IMAGE):latest ..

run: image        ## Run the image locally at http://127.0.0.1:8088 (production nginx config)
	docker run --rm -p 127.0.0.1:8088:8080 --read-only --tmpfs /tmp $(IMAGE):$(TAG)

push: image       ## Push both tags to the registry
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest
	@echo "pushed $(IMAGE):$(TAG)"

# Copies the compose file to the target via the management host, pins the tag in .env and restarts.
deploy: push
	printf 'SHARDPOST_WEBSITE_TAG=%s\n' '$(TAG)' > deploy/.env
	scp -q deploy/docker-compose.yml deploy/.env $(MGMT):/tmp/
	ssh $(MGMT) "sudo -n bash -c '$(ANSIBLE) -m ansible.builtin.file -a \"path=$(REMOTE_DIR) state=directory mode=0755\" >/dev/null \
	  && $(ANSIBLE) -m ansible.builtin.copy -a \"src=/tmp/docker-compose.yml dest=$(REMOTE_DIR)/docker-compose.yml mode=0644 backup=true\" >/dev/null \
	  && $(ANSIBLE) -m ansible.builtin.copy -a \"src=/tmp/.env dest=$(REMOTE_DIR)/.env mode=0644 backup=true\" >/dev/null \
	  && $(ANSIBLE) -m ansible.builtin.shell -a \"cd $(REMOTE_DIR) && docker compose pull -q && docker compose up -d --remove-orphans && docker compose ps\"; \
	  rm -f /tmp/docker-compose.yml /tmp/.env'"
	rm -f deploy/.env

clean:
	rm -rf dist
