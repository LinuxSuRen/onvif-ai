.PHONY: all run build test clean dev

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
