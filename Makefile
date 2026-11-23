.PHONY: run build fmt vet test docker-up docker-down clean

APP := ebook-api

run:
	go run ./cmd/$(APP)

build:
	mkdir -p bin
	go build -buildvcs=false -trimpath -o bin/$(APP) ./cmd/$(APP)

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

test:
	go test -race ./...

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

clean:
	rm -rf bin .tmp
