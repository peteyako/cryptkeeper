.PHONY: generate dev run build test vet docker linux-amd64 linux-arm64

generate:
	go tool templ generate

run: generate
	go run . serve -config config.yaml

# Live reload while editing templates (templ watches .templ files and proxies the app).
dev:
	go tool templ generate --watch --proxy="http://localhost:8787" --cmd="go run . serve -config config.yaml"

build: generate
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o crypto-portfolio .

test: generate
	go test -race ./...

vet:
	go vet ./...

linux-amd64: generate
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/crypto-portfolio-linux-amd64 .

linux-arm64: generate
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/crypto-portfolio-linux-arm64 .

docker:
	docker build -t vault-portfolio .
