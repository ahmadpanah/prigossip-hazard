# PriGossip-Hazard artifact build.
#
#   make build     -> bin/{agent,backend,workloadgen,tracegen}
#   make test      -> unit tests (hazard, gossip, dispatch, qos, agent)
#   make vet       -> go vet ./...
#   make traces    -> regenerate traces/ deterministically (tracegen)
#   make bpf       -> compile the eBPF objects (needs clang + libbpf; Linux)
#   make helm-lint -> validate the chart
#   make dist      -> release tarball with binaries + charts + traces

GO      ?= go
BIN     := bin
CMDS    := agent backend workloadgen tracegen
HELM    ?= helm

.PHONY: all build test vet traces bpf helm-lint dist clean

all: build

build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/ $(addprefix ./cmd/,$(CMDS))

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

traces: build
	./$(BIN)/tracegen -out traces

bpf:
	$(MAKE) -C bpf

helm-lint:
	$(HELM) lint deploy/helm/prigossip-hazard

dist: build
	@mkdir -p dist
	tar czf dist/prigossip-hazard-artifact.tar.gz \
	  --exclude bin --exclude dist --exclude .gopath \
	  LICENSE README.md Makefile go.mod go.sum cmd internal bpf deploy examples traces
	@echo "wrote dist/prigossip-hazard-artifact.tar.gz"

clean:
	rm -rf $(BIN) dist
	-$(MAKE) -C bpf clean
