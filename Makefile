BINARY := sznote
LDFLAGS := -s -w
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build run vet cross docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

run: build
	./$(BINARY)

vet:
	go vet ./...

# 交叉编译到 dist/，文件名形如 sznote_linux_amd64
cross:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo ">> $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" \
			-o "dist/sznote_$${os}_$${arch}$$ext" . || exit 1; \
	done

docker:
	docker build -t sznote:local .

clean:
	rm -rf dist $(BINARY)
