.PHONY: build build-worker test test-all serve clean

build:
	go build -trimpath -ldflags='-s -w' -o bin/distd ./cmd/distd

build-worker:
	GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o deploy/cloudflare/build/app.wasm ./cmd/worker

test:
	go test -race -count=1 ./...

test-all: test build build-worker
	python3 -m unittest -v tests/test_distribution_scripts.py

serve: build
	./bin/distd serve --listen :8080 --data-dir ./dist-data

clean:
	rm -rf bin deploy/cloudflare/build
