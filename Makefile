VERSION ?= 0.2.0
LDFLAGS := -s -w -X github.com/thisisnabi/Nube/internal/version.Version=$(VERSION)
export CGO_ENABLED := 0
export GOTOOLCHAIN := local

.PHONY: build linux test bundle clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nube ./cmd/nube

linux:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nube-linux-amd64 ./cmd/nube
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nube-linux-arm64 ./cmd/nube

test:
	go vet ./...
	go test ./...

bundle: linux
	mkdir -p dist/nube-$(VERSION)-linux
	cp dist/nube-linux-amd64 dist/nube-linux-arm64 deploy/install.sh dist/nube-$(VERSION)-linux/
	cp -r deploy/systemd examples dist/nube-$(VERSION)-linux/
	tar -czf dist/nube-$(VERSION)-linux.tar.gz -C dist nube-$(VERSION)-linux

clean:
	go clean
	$(RM) -r dist
