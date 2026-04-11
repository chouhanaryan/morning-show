.PHONY: build run dry-run test lint tidy clean

build:
	go build -o briefing ./cmd/briefing

run: build
	./briefing

dry-run: build
	./briefing --dry-run

verbose-dry-run: build
	./briefing --dry-run --verbose

test:
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy

clean:
	rm -f briefing
