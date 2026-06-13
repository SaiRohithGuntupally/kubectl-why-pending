BINARY := kubectl-why_pending
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build test vet demo install clean

build:
	go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

# Runs the full pipeline against a fake bare-metal cluster and prints a report.
demo:
	go test -run TestRun_EndToEnd -v . | sed -n '/^Pod /,/fix:/p'

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "Installed to $(INSTALL_DIR)/$(BINARY)"
	@echo "Ensure $(INSTALL_DIR) is on your PATH, then: kubectl why-pending --help"

# Cross-compile release tarballs + checksums into dist/ (cache cleared between
# targets to stay friendly on low-disk machines).
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
release:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; stage="dist/stage_$${os}_$${arch}"; \
	  mkdir -p "$$stage"; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w" -o "$$stage/$(BINARY)" . ; \
	  cp LICENSE README.md "$$stage/"; \
	  tar -C "$$stage" -czf "dist/kubectl-why-pending_$${os}_$${arch}.tar.gz" $(BINARY) LICENSE README.md; \
	  rm -rf "$$stage"; go clean -cache; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt && cat checksums.txt

clean:
	rm -rf $(BINARY) dist
