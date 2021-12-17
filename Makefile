#
#	Makefile
#
.PHONY: all
all: help

GO_VERSION:=	1.16
INSTALLER_SRC:=	/go/src/github.com/openshift/installer
MAKEFILE_PATH:=	$(abspath $(lastword $(MAKEFILE_LIST)))

PWD:=	$(patsubst %/,%,$(dir $(MAKEFILE_PATH)))
TARGET:=	$(if ${IS_CONTAINER},$@-container,$@)

BASH:=	$(shell which bash 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(BASH)
@echo "WARNING: bash is not installed"
endif

CUT:=	$(shell which cut 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(CUT)
@echo "WARNING: cut is not installed"
endif

GIT:=	$(shell which git 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(GIT)
@echo "WARNING: git is not installed"
endif

GREP:=	$(shell which grep 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(GREP)
@echo "WARNING: grep is not installed"
endif

GO:=	$(shell which go 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(GO)
@echo "WARNING: go is not installed"
endif

GPG:=	$(shell which gpg 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(GPG)
@echo "WARNING: gpg is not installed"
endif

PODMAN:=	$(shell which podman 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(PODMAN)
@echo "WARNING: podman is not installed"
endif

SED:=	$(shell which sed 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(PODMAN)
@echo "WARNING: sed is not installed"
endif

SHA256:=	$(shell which sha256sum 2>/dev/null|grep -v alias|awk '{ print $1 }')
ifndef $(SHA256)
@echo "WARNING: sha256sum is not installed"
endif

# Variables for the build target
MODE?=	release
SOURCE_GIT_COMMIT?=	$(shell $(GIT) rev-parse --verify 'HEAD^{commit}')
GIT_COMMIT:=	$(SOURCE_GIT_COMMIT)
BUILD_VERSION?=	$(shell $(GIT) describe --always --abbrev=40 --dirty)
GIT_TAG:=	$(BUILD_VERSION)
DEFAULT_ARCH?=	amd64
GOFLAGS?=	--mod=vendor
LDFLAGS+=	-X github.com/openshift/installer/pkg/version.Raw=$(GIT_TAG)
LDFLAGS+=	-X github.com/openshift/installer/pkg/version.Commit=$(GIT_COMMIT)
LDFLAGS+=	-X github.com/openshift/installer/pkg/version.defaultArch=$(DEFAULT_ARCH)
TAGS?=
OUTPUT?=	bin/openshift-install
CGO_ENABLED:=	0
SKIP_GENERATION?=

MINIMUM_GO_VERSION:=	1.17
CURRENT_GO_VERSION:=	$(subst go,,$(shell $(GO) version|$(CUT) -d' ' -f3))

MINIMUM_GO_VERSION_COOKED=	$(shell printf "%03d%03d%03d" $(subst ., ,$(MINIMUM_GO_VERSION)))
CURRENT_GO_VERSION_COOKED=	$(shell printf "%03d%03d%03d" $(subst ., ,$(CURRENT_GO_VERSION)))

.PHONY: go-version-check
go-version-check:
	@if [ "$(CURRENT_GO_VERSION_COOKED)" -lt "$(MINIMUM_GO_VERSION_COOKED)" ]; then \
		echo "ERROR: Go version should be greater or equal to $(MINIMUM_GO_VERSION)"; \
		exit 1; \
	fi


## build: Builds a release binary
build:
ifeq ($(MODE),release)
	$(eval LDFLAGS=$(LDFLAGS) -s -w)
	$(eval TAGS=$(TAGS) release)
else ifeq ($(MODE),dev)
	:
else
	$(error unrecognized mode: $(MODE))
endif
ifneq ($(SKIP_GENERATION),y)
	$(GO) generate ./data
endif
ifeq ($(findstring libvirt,$(TAGS)),libvirt)
	$(eval CGO_ENABLED=1)
endif
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -tags "$(TAGS)" -o "$(OUTPUT)" ./cmd/openshift-install


.PHONY: tag-build
tag-build:
	$(GIT) tag -sm "version $(TAG)" "$(TAG)"


.PHONY: build-release
build-release: build-release-darwin build-release-linux
	cd bin && $(SHA256) openshift-install-* > release.sha256
	cd bin && $(GPG) --output release.sha256.sig --detach-sig release.sha256


.PHONY: build-release-darwin-env
build-release-darwin-env:
	$(eval GOARCH=amd64)
	$(eval GOOS=darwin)
	$(eval OUTPUT=bin/openshift-install-$(GOOS)-$(GOARCH))
	$(eval SKIP_GENERATION=y)


build-release-darwin: build-release-darwin-env
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -tags "$(TAGS)" -o "$(OUTPUT)" ./cmd/openshift-install


.PHONY: build-release-linux-env
build-release-linux-env:
	$(eval GOARCH=amd64)
	$(eval GOOS=linux)
	$(eval OUTPUT=bin/openshift-install-$(GOOS)-$(GOARCH))
	$(eval SKIP_GENERATION=y)


build-release-linux: build-release-linux-env
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -tags "$(TAGS)" -o "$(OUTPUT)" ./cmd/openshift-install


## go-fmt: Formats go code
.PHONY: go-fmt
go-fmt:
	$(eval GO_FMT_ARGS=$(ARGS))
	@$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	/bin/sh -c 'for target in $(GO_FMT_ARGS); do \
		find $$target -name "*.go" ! -path "*/vendor/*" ! -path "*/.build/*" -print0 | xargs -0 gofmt -s -w ;\
	done'; \
	git diff --exit-code


## go-genmock: Generates mock code
.PHONY: go-genmock
go-genmock:
	$(eval GO_GENMOCK_ARGS=$(ARGS))
	$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	go install github.com/golang/mock/mockgen && \
	go generate ./pkg/asset/installconfig/... ${GO_GENMOCK_ARGS}


## go-lint: Lints go code
.PHONY: go-lint
go-lint:
	$(eval GO_LINT_ARGS=$(ARGS))
	$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	golint -set_exit_status ${GO_LINT_ARGS}


## go-sec: Check for security problems
.PHONY: go-sec
go-sec:
	$(eval GO_SEC_ARGS=$(ARGS))
	$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	/bin/sh -c 'go get github.com/securego/gosec/cmd/gosec && \
	gosec -severity high -confidence high -exclude G304 ./cmd/... ./data/... ./pkg/... ${GO_SEC_ARGS}'


## go-test: Run go tests
.PHONY: go-test
go-test:
	$(eval GO_TEST_ARGS=$(ARGS))
	$(PODMAN) run --rm \
	--env GO_TEST_ARGS="$(ARGS)" \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	go test ./cmd/... ./data/... ./pkg/... ${GO_TEST_ARGS}


## go-vet: Analyze go code
.PHONY: go-vet
go-vet:
	$(eval GO_VET_ARGS=$(ARGS))
	$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	go vet ${GO_VET_ARGS}


## release: Prepare for a release
.PHONY: release
release: tag-build build build-release


## shellcheck: Analyze shell scripts
.PHONY: shellcheck
shellcheck:
	$(eval TOP_DIR:=$(shell if [ -z "$(ARGS)" ]; then echo .; else echo "$(ARGS)"; fi))
	$(PODMAN) run --rm \
	--volume "$(PWD):/workdir:ro,z" \
	--workdir /workdir \
	quay.io/coreos/shellcheck-alpine:v0.5.0 \
	find "${TOP_DIR}" \
	-path "${TOP_DIR}/vendor" -prune \
	-o -path "${TOP_DIR}/.build" -prune \
	-o -path "${TOP_DIR}/tests/smoke/vendor" -prune \
	-o -path "${TOP_DIR}/tests/bdd-smoke/vendor" -prune \
	-o -path "${TOP_DIR}/tests/smoke/.build" -prune \
	-o -path "${TOP_DIR}/pkg/terraform/exec/plugins/vendor" -prune \
	-o -type f -name '*.sh' -exec shellcheck --format=gcc {} \+


## tf-fmt: Format terraform files
.PHONY: tf-fmt
tf-fmt:
	$(eval TF_FMT_ARGS:=$(shell if [ -z "$(ARGS)"]; then echo "-list -check -diff -write=false -recursive data/data"; else echo "$(ARGS)"; fi))
	$(PODMAN) run --rm \
	--volume "$(PWD):$(PWD):z" \
	--workdir "$(PWD)" \
	quay.io/coreos/terraform-alpine:v0.12.0-rc1 \
	terraform fmt ${TF_FMT_ARGS}


## tf-lint: Lint terraform files
.PHONY: tf-lint
tf-lint:
	$(PODMAN) run --rm \
	--volume "$(PWD):/data:z" \
	--entrypoint tflint \
	quay.io/coreos/tflint


## verify-codegen: Verify go generate is current
.PHONY: verify-codegen
verify-codegen:
	$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	set -xe ;\
	go generate ./pkg/types/installconfig.go ;\
	set +xe ;\
	git diff --exit-code


## verify-vendor: Verify go mod vendor is current
.PHONY: verify-vendor
verify-vendor:
	$(PODMAN) run --rm \
	--volume "$(PWD):$(INSTALLER_SRC):z" \
	--workdir $(INSTALLER_SRC) \
	docker.io/openshift/origin-release:golang-$(GO_VERSION) \
	set -euxo pipefail ;\
	go mod tidy ;\
	go mod vendor ;\
	go verify ;\
	go diff --exit-code


## yaml-lint: Lint YAML files
.PHONY: yaml-lint
yaml-lint:
	$(PODMAN) run --rm \
	--volume "$(PWD):/workdir:z" \
	--entrypoint yamllint \
	quay.io/coreos/yamllint \
	.


.PHONY: help
help:
	@echo
	@echo "Usage: make <target> ..."
	@echo "Where target in:"
	@$(GREP) -E '^##\s+(\w|\-)+:' Makefile | $(SED) -E -e 's|^##\s+(\w.+)$\|    \1|' -e 's|(\w.+)+:|\1:    |'
	@echo
	@echo "Target arguments can be set with ARGS:"
	@echo "    env ARGS=foo make <target>"
	@echo "    make <target> ARGS=foo" 
	@echo
