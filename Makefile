.PHONY: all run build test clean dev release

# === Release ===
# make release VERSION=0.0.1 —— 打 tag 并推送，触发 GitHub Actions：
# 多平台二进制 + ghcr.io Docker 镜像 + GitHub Release
release:
	@test -n "$(VERSION)" || (echo "usage: make release VERSION=0.0.1"; exit 1)
	git tag -a "v$(VERSION)" -m "release v$(VERSION)"
	git push origin "v$(VERSION)"

# === Server ===
run:
	@if [ -f .env ]; then export $$(grep -v '^#' .env | grep -v '^$$' | xargs); fi; go run ./cmd/server

build:
	CGO_ENABLED=1 go build -o bin/server ./cmd/server

test:
	go test ./... -v -count=1

test-unit:
	go test ./internal/... -v -count=1

test-e2e:
	cd web && npx playwright test

clean:
	rm -rf bin/

# === Frontend ===
web-dev:
	cd web && npm run dev

web-build:
	cd web && npm run build

# === Development ===
dev:
	@echo "Run in two terminals:"
	@echo "  Terminal 1: make run"
	@echo "  Terminal 2: make web-dev"

deps:
	go mod tidy
	cd web && npm install

# === Docker ===
docker-build:
	docker build -t onvif-ai .

docker-run:
	docker run -p 8080:8080 onvif-ai
