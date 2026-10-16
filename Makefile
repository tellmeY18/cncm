.PHONY: build web check run

web:
	cd web && npm ci && npm run build

build: web
	go build -o bin/cncm .

run: build
	./bin/cncm

check: web
	@test -z "$$(gofmt -l *.go)" || (gofmt -l *.go; exit 1)
	go vet ./...
	go test -race ./...
