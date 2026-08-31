SHELL := /bin/sh

APP_NAME := lower
SERVICE_NAME := $(APP_NAME).service

PROJECT_DIR := $(CURDIR)
BIN_DIR := $(PROJECT_DIR)/bin
BIN_PATH := $(BIN_DIR)/$(APP_NAME)
BIN_TMP := $(BIN_PATH).tmp
UNIT_FILE := $(PROJECT_DIR)/$(SERVICE_NAME)
SYSTEMD_UNIT := /etc/systemd/system/$(SERVICE_NAME)

MANAGED_MARKER := \# Managed-By: lawyer-bot Makefile lower.service

GO ?= go
GO_MAIN_PACKAGE ?= ./cmd
CGO_ENABLED ?= 0
GO_BUILD_FLAGS ?= -trimpath
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

SERVICE_USER ?= $(shell id -un)
SERVICE_GROUP ?= $(shell id -gn)
ENV_FILE ?= $(PROJECT_DIR)/.env
RESTART_SEC ?= 5s
STARTUP_CHECK_DELAY ?= 2s

SUDO ?= $(shell [ "$$(id -u)" = "0" ] || printf sudo)
SYSTEMCTL ?= systemctl
JOURNALCTL ?= journalctl
INSTALL ?= install
UNAME_S := $(shell uname -s 2>/dev/null || echo unknown)

.DEFAULT_GOAL := help
.NOTPARALLEL:

.PHONY: help build generate-unit verify-unit require-systemd assert-installed-safe run stop restart logs status uninstall clean

help:
	@printf '%s\n' "Targets:"
	@printf '%s\n' "  make build     Build $(BIN_PATH) and generate $(UNIT_FILE)"
	@printf '%s\n' "  make run       Install/update $(SERVICE_NAME), enable it, and start/restart it"
	@printf '%s\n' "  make stop      Stop only $(SERVICE_NAME)"
	@printf '%s\n' "  make restart   Rebuild, install/update the unit, and restart $(SERVICE_NAME)"
	@printf '%s\n' "  make status    Show systemd status for $(SERVICE_NAME)"
	@printf '%s\n' "  make logs      Follow logs with journalctl -u $(SERVICE_NAME) -f"
	@printf '%s\n' "  make uninstall Stop, disable, and remove only $(SYSTEMD_UNIT)"

build:
	@set -eu; \
	mkdir -p "$(BIN_DIR)"; \
	rm -f "$(BIN_TMP)"; \
	printf '%s\n' "Building $(BIN_PATH) from $(GO_MAIN_PACKAGE)..."; \
	CGO_ENABLED="$(CGO_ENABLED)" "$(GO)" build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o "$(BIN_TMP)" "$(GO_MAIN_PACKAGE)"; \
	mv "$(BIN_TMP)" "$(BIN_PATH)"; \
	chmod 0755 "$(BIN_PATH)"
	@$(MAKE) --no-print-directory generate-unit
	@$(MAKE) --no-print-directory verify-unit

generate-unit:
	@set -eu; \
	{ \
		printf '%s\n' "$(MANAGED_MARKER)"; \
		printf '%s\n' "# ProjectDirectory: $(PROJECT_DIR)"; \
		printf '%s\n' "# BinaryPath: $(BIN_PATH)"; \
		printf '%s\n' ""; \
		printf '%s\n' "[Unit]"; \
		printf '%s\n' "Description=Lower WhatsApp legal bot"; \
		printf '%s\n' "Wants=network-online.target"; \
		printf '%s\n' "After=network-online.target"; \
		printf '%s\n' ""; \
		printf '%s\n' "[Service]"; \
		printf '%s\n' "Type=simple"; \
		printf '%s\n' "User=$(SERVICE_USER)"; \
		printf '%s\n' "Group=$(SERVICE_GROUP)"; \
		printf '%s\n' "WorkingDirectory=$(PROJECT_DIR)"; \
		printf '%s\n' "Environment=ENV_FILE=$(ENV_FILE)"; \
		printf '%s\n' "ExecStart=$(BIN_PATH)"; \
		printf '%s\n' "Restart=always"; \
		printf '%s\n' "RestartSec=$(RESTART_SEC)"; \
		printf '%s\n' "KillSignal=SIGTERM"; \
		printf '%s\n' "TimeoutStopSec=30s"; \
		printf '%s\n' "StandardOutput=journal"; \
		printf '%s\n' "StandardError=journal"; \
		printf '%s\n' "SyslogIdentifier=$(APP_NAME)"; \
		printf '%s\n' "LimitNOFILE=65535"; \
		printf '%s\n' "NoNewPrivileges=true"; \
		printf '%s\n' "PrivateTmp=true"; \
		printf '%s\n' "ProtectSystem=full"; \
		printf '%s\n' "ReadWritePaths=$(PROJECT_DIR)"; \
		printf '%s\n' ""; \
		printf '%s\n' "[Install]"; \
		printf '%s\n' "WantedBy=multi-user.target"; \
	} > "$(UNIT_FILE)"; \
	printf '%s\n' "Generated $(UNIT_FILE)"

verify-unit:
	@set -eu; \
	test -x "$(BIN_PATH)" || { printf '%s\n' "ERROR: missing executable $(BIN_PATH)"; exit 1; }; \
	grep -Fqx "$(MANAGED_MARKER)" "$(UNIT_FILE)" || { printf '%s\n' "ERROR: generated unit is missing the safety marker"; exit 1; }; \
	grep -Fqx "WorkingDirectory=$(PROJECT_DIR)" "$(UNIT_FILE)" || { printf '%s\n' "ERROR: generated unit has the wrong WorkingDirectory"; exit 1; }; \
	grep -Fqx "ExecStart=$(BIN_PATH)" "$(UNIT_FILE)" || { printf '%s\n' "ERROR: generated unit has the wrong ExecStart"; exit 1; }; \
	if command -v systemd-analyze >/dev/null 2>&1; then \
		systemd-analyze verify "$(UNIT_FILE)"; \
	else \
		printf '%s\n' "systemd-analyze not found; skipped local unit syntax verification."; \
	fi

require-systemd:
	@set -eu; \
	if [ "$(UNAME_S)" != "Linux" ]; then \
		printf '%s\n' "ERROR: systemd targets require Linux; current OS is $(UNAME_S)."; \
		exit 1; \
	fi; \
	if ! command -v "$(SYSTEMCTL)" >/dev/null 2>&1; then \
		printf '%s\n' "ERROR: $(SYSTEMCTL) was not found."; \
		exit 1; \
	fi; \
	if [ ! -d /run/systemd/system ]; then \
		printf '%s\n' "ERROR: systemd does not appear to be running on this host."; \
		exit 1; \
	fi; \
	if [ "$$(id -u)" != "0" ] && [ -z "$(SUDO)" ]; then \
		printf '%s\n' "ERROR: system-level targets require root or SUDO=sudo."; \
		exit 1; \
	fi

assert-installed-safe:
	@set -eu; \
	if $(SUDO) test -e "$(SYSTEMD_UNIT)"; then \
		if ! $(SUDO) grep -Fqx "$(MANAGED_MARKER)" "$(SYSTEMD_UNIT)"; then \
			printf '%s\n' "ERROR: $(SYSTEMD_UNIT) already exists but was not generated by this Makefile."; \
			printf '%s\n' "Refusing to overwrite or modify an unrelated systemd service."; \
			exit 1; \
		fi; \
	fi

run:
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory require-systemd
	@$(MAKE) --no-print-directory assert-installed-safe
	@set -eu; \
	was_installed=0; \
	if $(SUDO) test -e "$(SYSTEMD_UNIT)"; then was_installed=1; fi; \
	printf '%s\n' "Installing $(SERVICE_NAME) to $(SYSTEMD_UNIT)..."; \
	$(SUDO) "$(INSTALL)" -m 0644 "$(UNIT_FILE)" "$(SYSTEMD_UNIT)"; \
	$(SUDO) "$(SYSTEMCTL)" daemon-reload; \
	$(SUDO) "$(SYSTEMCTL)" enable "$(SERVICE_NAME)"; \
	if [ "$$was_installed" -eq 1 ]; then \
		printf '%s\n' "Restarting existing $(SERVICE_NAME)..."; \
		$(SUDO) "$(SYSTEMCTL)" restart "$(SERVICE_NAME)"; \
	else \
		printf '%s\n' "Starting new $(SERVICE_NAME)..."; \
		$(SUDO) "$(SYSTEMCTL)" start "$(SERVICE_NAME)"; \
	fi; \
	sleep "$(STARTUP_CHECK_DELAY)"; \
	if ! $(SUDO) "$(SYSTEMCTL)" is-active --quiet "$(SERVICE_NAME)"; then \
		printf '%s\n' "ERROR: $(SERVICE_NAME) did not become active."; \
		$(SUDO) "$(SYSTEMCTL)" status --no-pager --full "$(SERVICE_NAME)" || true; \
		$(SUDO) "$(JOURNALCTL)" -u "$(SERVICE_NAME)" -n 80 --no-pager || true; \
		exit 1; \
	fi; \
	$(SUDO) "$(SYSTEMCTL)" status --no-pager --full "$(SERVICE_NAME)"

stop:
	@$(MAKE) --no-print-directory require-systemd
	@$(MAKE) --no-print-directory assert-installed-safe
	@set -eu; \
	if ! $(SUDO) test -e "$(SYSTEMD_UNIT)"; then \
		printf '%s\n' "$(SERVICE_NAME) is not installed at $(SYSTEMD_UNIT)."; \
		exit 0; \
	fi; \
	$(SUDO) "$(SYSTEMCTL)" stop "$(SERVICE_NAME)"

restart:
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory require-systemd
	@$(MAKE) --no-print-directory assert-installed-safe
	@set -eu; \
	printf '%s\n' "Installing updated $(SERVICE_NAME) to $(SYSTEMD_UNIT)..."; \
	$(SUDO) "$(INSTALL)" -m 0644 "$(UNIT_FILE)" "$(SYSTEMD_UNIT)"; \
	$(SUDO) "$(SYSTEMCTL)" daemon-reload; \
	$(SUDO) "$(SYSTEMCTL)" enable "$(SERVICE_NAME)"; \
	printf '%s\n' "Restarting $(SERVICE_NAME)..."; \
	$(SUDO) "$(SYSTEMCTL)" restart "$(SERVICE_NAME)"; \
	sleep "$(STARTUP_CHECK_DELAY)"; \
	if ! $(SUDO) "$(SYSTEMCTL)" is-active --quiet "$(SERVICE_NAME)"; then \
		printf '%s\n' "ERROR: $(SERVICE_NAME) did not become active after restart."; \
		$(SUDO) "$(SYSTEMCTL)" status --no-pager --full "$(SERVICE_NAME)" || true; \
		$(SUDO) "$(JOURNALCTL)" -u "$(SERVICE_NAME)" -n 80 --no-pager || true; \
		exit 1; \
	fi; \
	$(SUDO) "$(SYSTEMCTL)" status --no-pager --full "$(SERVICE_NAME)"

logs:
	@$(MAKE) --no-print-directory require-systemd
	@$(JOURNALCTL) -u "$(SERVICE_NAME)" -f

status:
	@$(MAKE) --no-print-directory require-systemd
	@$(SYSTEMCTL) status --no-pager --full "$(SERVICE_NAME)"

uninstall:
	@$(MAKE) --no-print-directory require-systemd
	@$(MAKE) --no-print-directory assert-installed-safe
	@set -eu; \
	if $(SUDO) test -e "$(SYSTEMD_UNIT)"; then \
		printf '%s\n' "Stopping and disabling $(SERVICE_NAME)..."; \
		$(SUDO) "$(SYSTEMCTL)" stop "$(SERVICE_NAME)" || true; \
		$(SUDO) "$(SYSTEMCTL)" disable "$(SERVICE_NAME)" || true; \
		printf '%s\n' "Removing $(SYSTEMD_UNIT)..."; \
		$(SUDO) rm -f "$(SYSTEMD_UNIT)"; \
	else \
		printf '%s\n' "$(SERVICE_NAME) is not installed at $(SYSTEMD_UNIT)."; \
	fi; \
	$(SUDO) "$(SYSTEMCTL)" daemon-reload

clean:
	@rm -f "$(BIN_TMP)" "$(BIN_PATH)" "$(UNIT_FILE)"
