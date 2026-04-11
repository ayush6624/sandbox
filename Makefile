# Remote deploy: set REMOTE_HOST (and optionally REMOTE_USER, REMOTE_DIR).
-include Makefile.local

REMOTE_USER ?= ayush
REMOTE_HOST ?= machine
REMOTE_DIR  ?= web-sandbox
GOARCH      ?= amd64

REMOTE      := $(REMOTE_USER)@$(REMOTE_HOST)
REMOTE_BASE := ssh -o BatchMode=yes $(REMOTE)
REMOTE_CD   := cd /home/$(REMOTE_USER)/$(REMOTE_DIR)

.PHONY: build build-linux sync sync-all remote-shell remote-setup remote-setup-devbox remote-up remote-down remote-doctor

build:
	go build ./...

build-linux:
	mkdir -p bin
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/websandbox ./cmd/websandbox

check-remote:
	@test -n "$(REMOTE_HOST)" || (echo "set REMOTE_HOST"; exit 1)

# --- Sync ---

sync: check-remote build-linux
	rsync -avz -e ssh \
		bin/websandbox \
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
	$(REMOTE_BASE) '$(REMOTE_CD) && ./bin/websandbox doctor'

# --- One-time setup ---

remote-setup: sync
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/setup-firecracker.sh'
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/setup-kernel.sh'

remote-setup-devbox: sync
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/build-devbox-rootfs.sh'
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo bash scripts/setup-network.sh'

# --- VM lifecycle ---

remote-up: check-remote
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./bin/websandbox up --config configs/devbox.json'

remote-down: check-remote
	$(REMOTE_BASE) '$(REMOTE_CD) && sudo ./bin/websandbox down --config configs/devbox.json'
