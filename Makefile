# Remote deploy: set REMOTE_HOST (and optionally REMOTE_USER, REMOTE_DIR).
-include Makefile.local

REMOTE_USER ?= ayush
REMOTE_HOST ?= machine
REMOTE_DIR  ?= sandbox
GOARCH      ?= amd64

REMOTE      := $(REMOTE_USER)@$(REMOTE_HOST)
REMOTE_BASE := ssh -o BatchMode=yes $(REMOTE)
REMOTE_CD   := cd /home/$(REMOTE_USER)/$(REMOTE_DIR)

.PHONY: build build-linux validate-infra sync sync-all remote-shell remote-setup remote-setup-devbox remote-install-agent remote-serve remote-up remote-down remote-list remote-doctor gcp-fleet-deploy gcp-fleet-status gcs-release fleet-rollout fleet-status fleet-rollout-local fleet-status-local

build:
	go build ./...

build-linux:
	mkdir -p bin
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/sandbox ./cmd/sandbox
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/sandboxd ./cmd/sandboxd
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/sandbox-edge ./services/sandbox-edge/cmd/sandbox-edge

validate-infra:
	bash infra/gcp/validate-scaling-owner.sh
	bash infra/gcp/startup-worker_test.sh
	bash infra/gcp/validate-control-deploy.sh
	bash infra/gcp/validate-edge-health.sh

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

# --- Autoscaling: publish binaries to GCS for the Nomad job to pull ---
# Uploads bin/{sandbox,sandboxd} under a git-sha prefix. The Nomad system job's
# artifact stanza fetches these; changing RELEASE_SHA in a deploy rolls the
# fleet. RELEASE_BUCKET comes from infra/gcp/config.env (or the environment).
# Sourced through `bash -c`, NOT bare $(shell ...): make runs that under /bin/sh,
# which is dash on Ubuntu, and config.env has a bash array (NAMES=(...)) that dash
# rejects outright — leaving RELEASE_BUCKET empty and failing the test below. It
# only ever worked on macOS, where /bin/sh is bash in posix mode and tolerates the
# array. Now that releases are published from the (Linux) control VM, this has to
# be explicit.
RELEASE_BUCKET ?= $(shell bash -c '. infra/gcp/config.env 2>/dev/null && echo $$RELEASE_BUCKET')
RELEASE_SHA    := $(shell git rev-parse --short HEAD)

gcs-release: build-linux
	@test -n "$(RELEASE_BUCKET)" || (echo "set RELEASE_BUCKET (infra/gcp/config.env)"; exit 1)
	@bash infra/gcp/require-control-vm.sh "make gcs-release"
	gsutil -q -m cp bin/sandbox bin/sandboxd bin/sandbox-edge gs://$(RELEASE_BUCKET)/releases/$(RELEASE_SHA)/
	@echo ">> published gs://$(RELEASE_BUCKET)/releases/$(RELEASE_SHA)/ — deploy with: infra/gcp/deploy-job.sh $(RELEASE_SHA)"

# --- One-command production rollout (prefer this over the three steps above) ---
# Builds+uploads, deploys only the components whose RUNNING release is stale
# (gateway first, then workers), waits for the gateway inventory to show every
# alive host on the target with capacity, then smoke-tests REST + the WebSocket
# pty. Idempotent. `make fleet-rollout SHA=<sha>` rolls a published release.
#
# These run ON THE CONTROL VM: the working tree is rsynced there and rollout.sh
# executes on it, so THIS machine never needs gcloud credentials — only ssh. A
# laptop `gcloud auth login` expires every session (Workspace Cloud session
# control) and cannot be swapped for a service-account key, because the org
# enforces iam.disableServiceAccountKeyCreation AND ...KeyUpload with no project
# override. The control VM runs as sandbox-control-sa with cloud-platform scope
# and takes credentials from the GCE metadata server: non-expiring, no prompt.
# rollout.sh itself refuses to run off-GCE, so this is enforced, not just
# documented. See infra/gcp/rollout-remote.sh.
fleet-rollout:
	bash infra/gcp/rollout-remote.sh $(SHA) $(ROLLOUT_ARGS)

fleet-status:
	bash infra/gcp/rollout-remote.sh --status

# Escape hatch: roll from THIS machine. Needs a live `gcloud auth login`, so
# expect it to fail with an expired token — prefer the targets above.
fleet-rollout-local:
	ROLLOUT_ALLOW_LOCAL=1 bash infra/gcp/rollout.sh $(SHA) $(ROLLOUT_ARGS)

fleet-status-local:
	ROLLOUT_ALLOW_LOCAL=1 bash infra/gcp/rollout.sh --status
