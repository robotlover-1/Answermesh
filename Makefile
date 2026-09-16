# AnswerMesh 根 Makefile（zrpc 迁移：Task 8）
#   make            # kvstore（保留原默认）
#   make zrpc       # 构建 third_party/zrpc/libzrpc.a（含 NtyCo）
#   make zrpc-test  # C 层测试（含 NtyCo 的普通套件）
#   make zrpc-sanitize  # 纯 C 测试(sanitizer)
#   make test-go    # 关键 Go 模块/用例（race）
#   make build      # 三个 Go 服务二进制 → bin/

.PHONY: all kvstore start stop clean zrpc zrpc-test zrpc-sanitize test-go build

all: kvstore

kvstore:
	$(MAKE) -C kvstore

start:
	./start.sh

stop:
	./stop.sh

clean:
	$(MAKE) -C kvstore clean

# ---- zrpc C 静态库 ----
zrpc:
	$(MAKE) -C third_party/zrpc

zrpc-test: zrpc
	$(MAKE) -C third_party/zrpc test

zrpc-sanitize:
	$(MAKE) -C third_party/zrpc clean >/dev/null 2>&1 || true
	CFLAGS="-O1 -g -fsanitize=address,undefined -fno-omit-frame-pointer" \
	  $(MAKE) -C third_party/zrpc test

# ---- 关键 Go 用例（race；真栈/含外部依赖的端到端由运维运行）----
test-go: zrpc ccli
	cd zrpc-go && CGO_ENABLED=1 go test -race ./...
	cd keywords-filter && CGO_ENABLED=1 go test ./filter-server/server/...
	cd ai-chat-service && CGO_ENABLED=1 go test ./chat-server/server/ ./services/keywords-filter/...
	cd ai-chat-backend && CGO_ENABLED=1 go test ./services/ai-chat-service/...

ccli:
	$(MAKE) -C third_party/zrpc ccli

# ---- 三个服务二进制 ----
build: zrpc
	mkdir -p bin
	cd keywords-filter && CGO_ENABLED=1 go build -o ../bin/keywords-filter ./filter-server
	cd ai-chat-service && CGO_ENABLED=1 go build -o ../bin/ai-chat-service ./chat-server
	cd ai-chat-backend && CGO_ENABLED=1 go build -o ../bin/ai-chat-backend ./cmd/
