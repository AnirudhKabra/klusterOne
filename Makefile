BINARY := ko-controller
CLI    := kubectl-nm
PKG    := github.com/AnirudhKabra/klusterOne
IMAGE  ?= ko-controller:dev

# Local toolchain bin dir (git-ignored). Used for downloaded codegen tools so
# that `make generate` / `make manifests` are hermetic and don't require the
# developer to pre-install anything.
LOCALBIN := $(shell pwd)/bin

# Pin controller-gen to a version compatible with k8s.io/api v0.29.x and
# Go 1.22+. Override on the command line: `make manifests CONTROLLER_GEN_VERSION=v0.16.5`.
CONTROLLER_GEN_VERSION ?= v0.16.5
CONTROLLER_GEN         := $(LOCALBIN)/controller-gen

.PHONY: tidy build cli run test fmt vet install-crd uninstall-crd docker install-cli generate manifests controller-gen deploy undeploy

tidy:
	go mod tidy

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/manager

cli:
	CGO_ENABLED=0 go build -o bin/$(CLI) ./cmd/kubectl-nm

# install-cli copies kubectl-nm into the first directory on $PATH that
# kubectl finds plugins in (commonly ~/.krew/bin or /usr/local/bin). Pass
# DEST=/some/dir to override.
DEST ?= /usr/local/bin
install-cli: cli
	install -m 0755 bin/$(CLI) $(DEST)/$(CLI)
	@echo "installed $(DEST)/$(CLI) — invoke as: kubectl nm <subcommand>"

run: build
	./bin/$(BINARY)

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

install-crd:
	kubectl apply -f config/crd/ko.io_nodemaintenances.yaml

uninstall-crd:
	kubectl delete -f config/crd/ko.io_nodemaintenances.yaml --ignore-not-found

docker:
	docker build -t $(IMAGE) .

# deploy installs the CRD, ServiceAccount, (Cluster)Role(Binding)s, Namespace,
# and the controller Deployment using kustomize. The controller image is
# overridable: `make deploy IMAGE=ghcr.io/kluster-one/ko-controller:v0.1.0`.
deploy:
	cd config && kustomize edit set image ko-controller=$(IMAGE)
	kubectl apply -k config

# undeploy removes everything `make deploy` installed. The CRD deletion will
# block until all NodeMaintenance objects are gone.
undeploy:
	kubectl delete -k config --ignore-not-found

# controller-gen downloads the pinned controller-gen into ./bin on first use.
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN):
	@mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	@echo "installed $(CONTROLLER_GEN) ($(CONTROLLER_GEN_VERSION))"

# generate regenerates api/v1alpha1/zz_generated.deepcopy.go from the Go types.
# Run this whenever you add or change a field in nodemaintenance_types.go.
generate: controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."

# manifests regenerates config/crd/*.yaml from the +kubebuilder markers on the
# Go types. Run this whenever you change fields, validation, or printer columns.
#
# The post-processing sed strips `format: int32` / `format: int64` lines that
# controller-gen emits for integer fields. They are valid OpenAPI v3 but
# kubectl's client-side validator does not recognize them and prints
# `Warning: unrecognized format "int64"` on every apply. The API server only
# uses `type: integer` for validation, so removing the format hint is safe
# and purely cosmetic.
manifests: controller-gen
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=config/crd
	@find config/crd -name '*.yaml' -exec sed -i -E '/^[[:space:]]*format: int(32|64)$$/d' {} +
