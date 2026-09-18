# Makefile — thin wrapper over Taskfile.yml (the canonical runner, issue #299).
# `task --list` shows the full set; these mirrors exist only for muscle memory.

.PHONY: all build web-build build-proxy test test-fast test-pool test-server test-e2e test-race lint web-dev dev-proxy verify verify-quick verify-full clean

all: build

web-build:
	task frontend:build

build-proxy:
	task build:proxy

build:
	task build

test:
	task test

test-fast:
	task test:fast

test-pool:
	task test:pool

test-server:
	task test:server

test-e2e:
	task test:e2e

test-race:
	task test:race

lint:
	task lint

web-dev:
	task frontend:dev

dev-proxy:
	task dev

clean:
	rm -rf bin

verify:
	task verify

verify-quick:
	task verify:quick

verify-full:
	task verify:full
