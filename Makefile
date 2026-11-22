SHELL := /bin/bash

VERSION ?= $(shell cat VERSION)
PLUGIN := phoenix
LDFLAGS := -X main.pluginVersion=$(VERSION)

.PHONY: test build check-version release clean

test:
	go test ./...

check-version:
	@test "$(VERSION)" = "$$(cat VERSION)" || (echo "VERSION mismatch: $(VERSION) != $$(cat VERSION)"; exit 1)
	@grep -q 'pluginVersion.*= "$(VERSION)"' main.go || (echo "main.go default version mismatch"; exit 1)
	@grep -q '^## $(VERSION) ' CHANGELOG.md || (echo "CHANGELOG.md entry missing for $(VERSION)"; exit 1)

build: test check-version
	@mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags '$(LDFLAGS)' -o dist/$(PLUGIN).so .

release: test check-version
	@mkdir -p dist
	@rm -f dist/$(PLUGIN)_$(VERSION)_$$(go env GOOS)_$$(go env GOARCH).zip dist/checksums.txt
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags '$(LDFLAGS)' -o dist/$(PLUGIN).so .
	@zip -j dist/$(PLUGIN)_$(VERSION)_$$(go env GOOS)_$$(go env GOARCH).zip dist/$(PLUGIN).so
	@sha256sum dist/$(PLUGIN)_$(VERSION)_$$(go env GOOS)_$$(go env GOARCH).zip > dist/checksums.txt
	@rm -f dist/$(PLUGIN).so

clean:
	rm -rf dist
