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

clean:
	rm -f $(BINARY)
