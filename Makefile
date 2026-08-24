.PHONY: build run test tidy clean key

build:
	go build -o bin/server ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

tidy:
	go mod tidy

key:
	@openssl rand -base64 32

clean:
	rm -rf bin *.db *.db-wal *.db-shm
