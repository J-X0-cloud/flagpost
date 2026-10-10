.PHONY: build test race bench lint fmt run clean

build:
	go build -trimpath -o bin/flagpost ./cmd/flagpost

test:
	go test ./...

race:
	go test -race -count=1 ./...

bench:
	go test -run '^$$' -bench . -benchmem ./eval/ ./sdk/

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	staticcheck ./...

fmt:
	gofmt -w .

run: build
	FLAGPOST_ADMIN_TOKEN=$${FLAGPOST_ADMIN_TOKEN:-dev-admin-token-change-me} \
	FLAGPOST_BOOTSTRAP_ENVS=$${FLAGPOST_BOOTSTRAP_ENVS:-production,staging} \
	./bin/flagpost serve

clean:
	rm -rf bin
