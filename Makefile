.PHONY: help build test vet lint shellcheck clean \
        install \
        install-check install-kernel install-init install-rootfs install-firecracker install-host-kernel \
        check-patches checkout-upstream generate-patches normalize-patches bump-upstream \
        docs docs-check \
        setup provision \
        build-firecracker build-host-kernel build-kernel build-rootfs \
        build-qemu build-qemu-patched build-gvisor \
        deploy deploy-example rollback status clean-stamps

BACKEND          ?= tcg
FORCE            ?=
BUILD_FIRECRACKER ?=
BUILD_KERNEL     ?=
BUILD_ROOTFS     ?=
HOST_KERNEL      ?=
COMPONENT        ?=
NEW_VERSION      ?=

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -trimpath -ldflags "-s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)"

DEPLOY := ./deploy

# Remote deployment - set HOST=user@ip
HOST     ?=
EXAMPLE  ?=
_HOST_KEY = $(subst .,_,$(subst @,_,$(HOST)))
STAMPS   := .make-stamps/$(_HOST_KEY)

help:
	@echo "Usage: make <target> [HOST=user@ip] [EXAMPLE=kv] [BACKEND=tcg|firecracker] [FORCE=1]"
	@echo ""
	@echo "Local:"
	@echo "  build              Build openthesis for the current platform"
	@echo "  install            Install all local artifacts (downloads pre-built when available)"
	@echo "  install-check      Check local prerequisites (Docker, QEMU, /dev/kvm)"
	@echo "  install-kernel     Download or build guest kernel (Linux ${KERNEL_VERSION} + DST patches)"
	@echo "  install-init       Download or build openthesis-init guest agent binary"
	@echo "  install-rootfs     Download or build Debian bookworm guest rootfs"
	@echo "  install-firecracker Download pre-built patched Firecracker or stock fallback"
	@echo "  install-host-kernel Patch host KVM modules for RDTSC+HLT exits (sudo, ~10 min)"
	@echo "  check-patches      Verify patch series consistency (fast, no clone)"
	@echo "  checkout-upstream  Clone upstream and apply patches for editing (COMPONENT=firecracker|linux|...)"
	@echo "  generate-patches   Regenerate patch files from upstream working tree"
	@echo "  normalize-patches  Fix patch hunk counts: apply --recount then regenerate (one-shot fix)"
	@echo "  bump-upstream      Rebase patches onto new upstream version (also needs NEW_VERSION=v1.x.y)"
	@echo "  test               Run tests"
	@echo "  vet                Run go vet"
	@echo "  lint               Run golangci-lint"
	@echo "  shellcheck         Run shellcheck on deploy/ shell scripts"
	@echo "  docs               Regenerate site/docs/reference/cli.md from CLI source"
	@echo "  docs-check         Fail if cli.md is stale (use in CI)"
	@echo "  clean              Remove build artifacts"
	@echo ""
	@echo "  Pass FORCE=1 to any install-* target to rebuild even if output already exists."
	@echo "  Pass BACKEND=firecracker to install Firecracker on Linux (default: tcg)."
	@echo "  Pass BUILD_KERNEL=1 / BUILD_ROOTFS=1 to skip download and build from source."
	@echo "  Pass HOST_KERNEL=1 to also patch host KVM modules (requires sudo)."
	@echo ""
	@echo "Remote setup (requires HOST=user@ip):"
	@echo "  setup              Full first-time setup (provision + build all + deploy)"
	@echo "  provision          Provision bare-metal server"
	@echo "  build-firecracker  Build patched Firecracker on server (~30-60 min)"
	@echo "  build-host-kernel  Build patched KVM modules on server (~10 min)"
	@echo "  build-kernel       Build deterministic guest kernel on server (~20 min)"
	@echo "  build-rootfs       Build guest rootfs on server (~10 min)"
	@echo "  build-qemu         Build stock QEMU for TCG backend on server"
	@echo "  build-qemu-patched Build patched QEMU for patched backend on server"
	@echo "  build-gvisor       Build patched gVisor for gvisor backend on server"
	@echo "  deploy             Cross-compile and deploy openthesis to server"
	@echo "  deploy-example     Deploy an example (also requires EXAMPLE=kv|etcd|redis|nats|postgres)"
	@echo "  rollback           Roll back to previous binary"
	@echo "  status             Show completed setup steps for HOST"
	@echo "  clean-stamps       Clear cached step status for HOST"
	@echo ""
	@echo "Re-run a completed step:  make clean-stamps HOST=... && make <step> HOST=..."

install:
	BACKEND=$(BACKEND) FORCE=$(FORCE) HOST_KERNEL=$(HOST_KERNEL) $(DEPLOY)/local/install

install-check:
	$(DEPLOY)/local/check

install-kernel:
	FORCE=$(FORCE) BUILD_KERNEL=$(BUILD_KERNEL) $(DEPLOY)/local/kernel

install-init:
	FORCE=$(FORCE) $(DEPLOY)/local/init

install-rootfs:
	FORCE=$(FORCE) BUILD_ROOTFS=$(BUILD_ROOTFS) $(DEPLOY)/local/rootfs

install-firecracker:
	FORCE=$(FORCE) BUILD_FIRECRACKER=$(BUILD_FIRECRACKER) $(DEPLOY)/local/firecracker

install-host-kernel:
	$(DEPLOY)/server/build-host-kernel.sh --local

fix-host-kernel-patches:
	$(DEPLOY)/local/fix-host-kernel-patches /usr/src/linux-source-6.18.7

check-patches:
	$(DEPLOY)/local/check-patches

checkout-upstream:
	@[ -n "$(COMPONENT)" ] || (echo "error: COMPONENT is required (e.g. COMPONENT=firecracker)"; exit 1)
	$(DEPLOY)/local/checkout-upstream $(COMPONENT)

generate-patches:
	@[ -n "$(COMPONENT)" ] || (echo "error: COMPONENT is required (e.g. COMPONENT=firecracker)"; exit 1)
	$(DEPLOY)/local/generate-patches $(COMPONENT)

normalize-patches:
	@[ -n "$(COMPONENT)" ] || (echo "error: COMPONENT is required (e.g. COMPONENT=firecracker)"; exit 1)
	$(DEPLOY)/local/normalize-patches $(COMPONENT)

bump-upstream:
	@[ -n "$(COMPONENT)" ] || (echo "error: COMPONENT and NEW_VERSION are required"; exit 1)
	@[ -n "$(NEW_VERSION)" ] || (echo "error: NEW_VERSION is required (e.g. NEW_VERSION=v1.16.0)"; exit 1)
	$(DEPLOY)/local/bump-upstream $(COMPONENT) $(NEW_VERSION)

_require-host:
	@[ -n "$(HOST)" ] || (echo "error: HOST is required (e.g. HOST=ubuntu@1.2.3.4)"; exit 1)

$(STAMPS):
	@mkdir -p $@

setup: _require-host $(STAMPS)/provision $(STAMPS)/build-firecracker $(STAMPS)/build-host-kernel $(STAMPS)/build-kernel $(STAMPS)/build-rootfs $(STAMPS)/deploy

$(STAMPS)/provision: | _require-host $(STAMPS)
	$(DEPLOY)/server/provision.sh $(HOST)
	@touch $@

$(STAMPS)/build-firecracker: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-firecracker.sh $(HOST)
	@touch $@

$(STAMPS)/build-host-kernel: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-host-kernel.sh $(HOST)
	@touch $@

$(STAMPS)/build-kernel: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-kernel.sh $(HOST)
	@touch $@

$(STAMPS)/build-rootfs: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-rootfs.sh $(HOST)
	@touch $@

$(STAMPS)/deploy: | _require-host $(STAMPS)
	$(DEPLOY)/server/deploy.sh $(HOST)
	@touch $@

$(STAMPS)/build-qemu: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-qemu.sh $(HOST)
	@touch $@

$(STAMPS)/build-qemu-patched: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-qemu-patched.sh $(HOST)
	@touch $@

$(STAMPS)/build-gvisor: | _require-host $(STAMPS)
	$(DEPLOY)/server/build-gvisor.sh $(HOST)
	@touch $@

provision: $(STAMPS)/provision
build-firecracker: $(STAMPS)/build-firecracker
build-host-kernel: $(STAMPS)/build-host-kernel
build-kernel: $(STAMPS)/build-kernel
build-rootfs: $(STAMPS)/build-rootfs
build-qemu: $(STAMPS)/build-qemu
build-qemu-patched: $(STAMPS)/build-qemu-patched
build-gvisor: $(STAMPS)/build-gvisor
deploy: $(STAMPS)/deploy

deploy-example: _require-host
	@[ -n "$(EXAMPLE)" ] || (echo "error: EXAMPLE is required (e.g. EXAMPLE=kv)"; exit 1)
	$(DEPLOY)/server/deploy-example.sh $(HOST) $(EXAMPLE)

rollback: _require-host
	$(DEPLOY)/server/rollback.sh $(HOST)

status: _require-host
	@echo "Setup status for $(HOST):"
	@for step in provision build-firecracker build-host-kernel build-kernel build-rootfs build-qemu build-qemu-patched build-gvisor deploy; do \
		if [ -f "$(STAMPS)/$$step" ]; then \
			printf "  [x] $$step\n"; \
		else \
			printf "  [ ] $$step\n"; \
		fi; \
	done

clean-stamps: _require-host
	@rm -rf $(STAMPS)
	@echo "Cleared stamps for $(HOST)"

docs:
	go run ./cmd/docgen > site/docs/reference/cli.md

docs-check:
	@go run ./cmd/docgen | diff - site/docs/reference/cli.md || \
		(echo "\nCLI reference is stale. Run 'make docs' to regenerate."; exit 1)

build:
	go build $(LDFLAGS) -o bin/openthesis ./cmd/openthesis

test:
	go test ./... -race -count=1

vet:
	go vet ./...

lint:
	golangci-lint run

shellcheck:
	@command -v shellcheck >/dev/null 2>&1 || { echo "shellcheck not installed: sudo apt install shellcheck  or  brew install shellcheck"; exit 1; }
	shellcheck -S warning \
	    deploy/local/check deploy/local/install deploy/local/kernel deploy/local/rootfs \
	    deploy/local/init deploy/local/firecracker deploy/local/check-patches \
	    deploy/local/checkout-upstream deploy/local/generate-patches \
	    deploy/local/normalize-patches deploy/local/bump-upstream \
	    deploy/server/build-kernel.sh deploy/server/build-rootfs.sh \
	    deploy/server/build-firecracker.sh deploy/server/build-host-kernel.sh

clean:
	rm -rf bin/
