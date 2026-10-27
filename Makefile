.PHONY: build web check run kite-manifest

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

# manifests/cncm-containers-anywhere.yaml is the single paste for Kite on Containers
# Anywhere 0.2.0+: the manager manifest with auto origin, plus its publication.
kite-manifest:
	{ printf '%s\n' '# Generated from cncm.yaml and cncm-publication.yaml (CNCM_ORIGIN=auto); do not edit.' '# Paste this one file into Kite on Containers Anywhere 0.2.0+.'; \
	  sed "s|value: 'http://localhost:8080'|value: auto|" manifests/cncm.yaml; echo '---'; cat manifests/cncm-publication.yaml; } > manifests/cncm-containers-anywhere.yaml
