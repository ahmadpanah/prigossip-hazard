# SPDX-License-Identifier: Apache-2.0
#
# Builds the PriGossip-Hazard eBPF objects. Requires clang (>= 12), a BPF
# capable target and kernel headers or a generated vmlinux.h.
#
#   make            # build all objects into ./build
#   make install    # copy objects to /var/lib/prigossip-hazard/bpf
#   make clean

CLANG      ?= clang
CCFLAGS    := -g -O2 -target bpf -D__TARGET_ARCH_x86 -Wall -Werror \
              -I$(CURDIR)/include -I/usr/include/$(shell uname -m)-linux-gnu
BUILDDIR   := $(CURDIR)/build
OBJECTS    := $(BUILDDIR)/sock_ops.o $(BUILDDIR)/sk_msg.o $(BUILDDIR)/kprobe_latency.o
INSTALLDIR ?= /var/lib/prigossip-hazard/bpf

.PHONY: all install clean

all: $(OBJECTS)

$(BUILDDIR):
	mkdir -p $(BUILDDIR)

$(BUILDDIR)/sock_ops.o: sock_ops.c common.h | $(BUILDDIR)
	$(CLANG) $(CCFLAGS) -c $< -o $@

$(BUILDDIR)/sk_msg.o: sk_msg.c common.h | $(BUILDDIR)
	$(CLANG) $(CCFLAGS) -c $< -o $@

$(BUILDDIR)/kprobe_latency.o: kprobe_latency.c common.h | $(BUILDDIR)
	$(CLANG) $(CCFLAGS) -c $< -o $@

install: all
	install -d $(INSTALLDIR)
	install -m 0644 $(OBJECTS) $(INSTALLDIR)/

clean:
	rm -rf $(BUILDDIR)
