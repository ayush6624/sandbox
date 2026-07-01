# Remote deploy: set REMOTE_HOST (and optionally REMOTE_USER, REMOTE_DIR).
-include Makefile.local

REMOTE_USER ?= ayush
REMOTE_HOST ?= machine
REMOTE_DIR  ?= sandbox
GOARCH      ?= amd64

REMOTE      := $(REMOTE_USER)@$(REMOTE_HOST)
REMOTE_BASE := ssh -o BatchMode=yes $(REMOTE)
REMOTE_CD   := cd /home/$(REMOTE_USER)/$(REMOTE_DIR)

.PHONY: build build-linux sync sync-all remote-shell remote-setup remote-setup-devbox remote-install-agent remote-serve remote-up remote-down remote-list remote-doctor gcp-fleet-deploy gcp-fleet-status

build:
	go build ./...

build-linux:
	mkdir -p bin
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/sandbox ./cmd/sandbox
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/sandboxd ./cmd/sandboxd

check-remote:
	@test -n "$(REMOTE_HOST)" || (echo "set REMOTE_HOST"; exit 1)

# --- Sync ---

sync: check-remote build-linux
	rsync -avz -e ssh \
		bin/sandbox \
		bin/sandboxd \
		Makefile \
		configs \
		scripts \
		$(REMOTE):/home/$(REMOTE_USER)/$(REMOTE_DIR)/

sync-all: check-remote build-linux
	rsync -avz -e ssh \
		./ $(REMOTE):/home/$(REMOTE_USER)/$(REMOTE_DIR)/ \
		--exclude .git --exclude bin

# --- Remote commands ---

remote-shell: check-remote
	ssh $(REMOTE)

remote-doctor: check-remote
	$(REMOTE_BASE) '$(REMOTE_CD) && ./sandbox doctor --config configs/devbox.json'

# --- One-time setup ---

remote-setup: sync
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/setup-firecracker.sh'
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/setup-kernel.sh'

remote-setup-devbox: sync
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/build-devbox-rootfs.sh'
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/setup-network.sh'

# Install/update the sandboxd guest agent inside the base rootfs.
remote-install-agent: sync
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./sandbox install-agent --agent ./sandboxd'

# --- Server + sandbox lifecycle ---

remote-serve: check-remote
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./sandbox serve --config configs/devbox.json'

remote-up: check-remote
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./sandbox up --config configs/devbox.json'

# Usage: make remote-down SANDBOX=<id>
remote-down: check-remote
	@test -n "$(SANDBOX)" || (echo "set SANDBOX=<id>"; exit 1)
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./sandbox down $(SANDBOX) --config configs/devbox.json'

remote-list: check-remote
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./sandbox list --config configs/devbox.json'

# --- GCP fleet (testvm-1/2): build + bootstrap every host + install systemd units ---
# Distinct from the single-host remote-* targets above (those target REMOTE_HOST).
# See infra/gcp/fleet-deploy.sh and memory gcp-sandbox-fleet.
gcp-fleet-deploy:
	bash infra/gcp/fleet-deploy.sh deploy

gcp-fleet-status:
	bash infra/gcp/fleet-deploy.sh status
