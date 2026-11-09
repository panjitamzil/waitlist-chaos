.PHONY: build screenshots test test-integration vet migrate-up migrate-down migrate-status experiment experiment-verbose db-local-up db-local-down clean

build:
	go build -o bin/ ./cmd/...

vet:
	go vet ./...
	go vet -tags integration ./...

test:
	go test ./...

test-integration:
	go test -tags integration -count=1 -p 1 -v ./...

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

migrate-status:
	go run ./cmd/migrate status

experiment: build
	./bin/experiment

experiment-verbose: build
	./bin/experiment -v

screenshots: build
	./docs/demo/render.sh

db-local-up:
	docker compose up -d --wait

db-local-down:
	docker compose down

clean:
	rm -rf bin
