.PHONY: check fmt fmt-check vet lint test build vuln container smoke

check: fmt-check vet lint test build

fmt:
	gofmt -w cmd internal pkg

fmt-check:
	@test -z "$$(gofmt -l cmd internal pkg)" || { gofmt -l cmd internal pkg; exit 1; }

vet:
	go vet ./...

lint:
	go tool staticcheck ./...
	go tool actionlint .github/workflows/ci.yml

test:
	go test -race -coverprofile=coverage.out ./...

build:
	go build -trimpath -o bin/gitlab-runner-controller ./cmd/gitlab-runner-controller

vuln:
	go tool govulncheck ./...

container: build
	@release_version="$$(./bin/gitlab-runner-controller version)" && \
		docker build --build-arg VERSION="$$release_version" -t gitlab-runner-controller:local .

smoke: build
	@release_version="$$(./bin/gitlab-runner-controller version)" && \
		docker build --build-arg VERSION="$$release_version" --target smoke -t gitlab-runner-controller:smoke .
	bash scripts/smoke-container.sh gitlab-runner-controller:smoke
