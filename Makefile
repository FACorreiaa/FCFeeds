.PHONY: build test lint run docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-w -s" -o bin/feeds ./cmd/feeds

test:
	go test -race -count=1 ./...

lint:
	gofmt -l . && go vet ./...

run: build
	mkdir -p data && FEEDS_DB_PATH=data/feeds.db FEEDS_ADDR=:8080 ./bin/feeds

docker:
	docker build -t feeds:dev .
