.DEFAULT_GOAL := help

GO ?= go
POWERSHELL ?= powershell.exe
DOCKER ?= docker
CONFIGURATION ?= Release
ARCHITECTURE ?= all
ALTITUDE ?=
MINIFILTER_IMAGE ?= vfs-sandbox-minifilter:ewdk
DOCKER_BUILD_MEMORY ?= 4g
WINDOWS_BASE ?= mcr.microsoft.com/windows/servercore:ltsc2022
WINDOWS_ISOLATION ?= process
COMPOSE_FILE := drivers/minifilter/docker/compose.yaml
PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-386

.PHONY: help build cross $(PLATFORMS) minifilter minifilter-image minifilter-docker
help:
	@echo 'make build                  Build the Go launcher for the current platform'
	@echo 'make cross                  Build launchers for all supported build targets'
	@echo 'make windows-386            Build one launcher target (see PLATFORMS)'
	@echo 'make minifilter ALTITUDE=... [ARCHITECTURE=x64|x86|all]'
	@echo '                            Build drivers in an initialized Windows WDK shell'
	@echo 'make minifilter-image       Build the Windows EWDK container image'
	@echo 'make minifilter-docker ALTITUDE=... [ARCHITECTURE=x64|x86|all]'
	@echo '                            Build drivers using a Windows Docker engine'
	@echo 'CONFIGURATION=Release|Debug; Docker setup: drivers/minifilter/docker/README.md'

build:
	@mkdir -p dist
	$(GO) build -trimpath -o "dist/vfs-sandbox$$($(GO) env GOEXE)" .

cross: $(PLATFORMS)

$(PLATFORMS):
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$@)) GOARCH=$(word 2,$(subst -, ,$@)) $(GO) build -trimpath -o dist/vfs-sandbox-$@$(if $(filter windows-%,$@),.exe,) .

# GNU Make with a POSIX shell (Git Bash on Windows); build.ps1 validates inputs.
minifilter:
	@test -n "$(ALTITUDE)" || { echo 'ALTITUDE is required'; exit 2; }
	@case "$(ARCHITECTURE)" in x64|x86) architectures="$(ARCHITECTURE)" ;; all) architectures='x64 x86' ;; *) echo 'ARCHITECTURE must be x64, x86 or all'; exit 2 ;; esac; \
	for arch in $$architectures; do \
		$(POWERSHELL) -NoProfile -NonInteractive -ExecutionPolicy Bypass -File drivers/minifilter/build.ps1 -Altitude "$(ALTITUDE)" -Architecture "$$arch" -Configuration "$(CONFIGURATION)" || exit $$?; \
	done

minifilter-image:
	@test "$$($(DOCKER) info --format '{{.OSType}}')" = windows || { echo 'A Windows Docker engine is required'; exit 2; }
	$(DOCKER) build --memory "$(DOCKER_BUILD_MEMORY)" --isolation "$(WINDOWS_ISOLATION)" --build-arg "WINDOWS_BASE=$(WINDOWS_BASE)" -t "$(MINIFILTER_IMAGE)" drivers/minifilter/docker

minifilter-docker:
	@test -n "$(ALTITUDE)" || { echo 'ALTITUDE is required'; exit 2; }
	@test "$$($(DOCKER) info --format '{{.OSType}}')" = windows || { echo 'A Windows Docker engine is required'; exit 2; }
	@mkdir -p dist/minifilter
	ALTITUDE="$(ALTITUDE)" ARCHITECTURE="$(ARCHITECTURE)" CONFIGURATION="$(CONFIGURATION)" MINIFILTER_IMAGE="$(MINIFILTER_IMAGE)" WINDOWS_ISOLATION="$(WINDOWS_ISOLATION)" $(DOCKER) compose -f $(COMPOSE_FILE) run --rm --no-deps builder
