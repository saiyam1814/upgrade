.PHONY: build test install clean refresh-rules vet fmt

VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%d)
LDFLAGS := -X github.com/saiyam1814/upgrade/internal/cmd.version=$(VERSION) \
           -X github.com/saiyam1814/upgrade/internal/cmd.commit=$(COMMIT) \
           -X github.com/saiyam1814/upgrade/internal/cmd.dataDate=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/kubectl-upgrade .

install:
	go install -ldflags "$(LDFLAGS)" .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

# Refresh the embedded pluto deprecation data. Run this before tagging
# a release or whenever pluto cuts a new versions.yaml.
refresh-rules:
	curl -sSL https://raw.githubusercontent.com/FairwindsOps/pluto/master/versions.yaml \
	    -o internal/rules/apis/versions.yaml
	@echo "rules refreshed; commit internal/rules/apis/versions.yaml and bump VERSION."

clean:
	rm -rf bin/

# Smoke test against the bundled fixture.
smoke: build
	./bin/kubectl-upgrade scan --target v1.33 --source manifests --path testdata/manifests/ --fail-on none
	./bin/kubectl-upgrade plan --from v1.30 --to v1.34
	./bin/kubectl-upgrade simulate --from v1.31 --to v1.34
	./bin/kubectl-upgrade vcluster --explain

# Lint with go vet + standard checks
lint: vet
	gofmt -l . | grep . && exit 1 || echo "fmt clean"
