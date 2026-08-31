# Development entry points. `make` with no target lists them.
#
# Every target that needs configuration reads .env, so no environment has to be typed
# and nothing here is shell-specific: the same commands work from cmd.exe, PowerShell,
# and a POSIX shell. Before this file, the environment lived in the README as a
# PowerShell block, which cmder cannot run at all -- the failure was a message about
# directory syntax, so the reader looks at the DSN rather than at the shell.
#
# .env is loaded by make and exported to the child process. The service itself still
# reads only the process environment, so nothing about a deployment changes.

SHELL := cmd.exe
.SHELLFLAGS := /c

# -include, not include: every target that does not need configuration (fmt, vet,
# build, test-unit) must work in a fresh clone with no .env, and in CI, where the
# environment comes from the workflow rather than from a file.
-include .env
export

ADDR ?= 127.0.0.1:8099
ISSUER ?= 127.0.0.1:8098
TOKEN_FILE ?= .token

# ROLE=tenant mints a Tenant-scoped token instead. Worth having as a target rather than
# a note: a Tenant token on a provider route is refused 403, and seeing that for
# yourself is the difference between trusting the boundary and assuming it.
ROLE ?= provider

# The Tenant a ROLE=tenant token administers. Defaults to the seeded Tenant A; override with
# TENANT=<uuid>. Required by the issuer, which is why it is a variable rather than a note:
# `make token ROLE=tenant` without it was refused, and the refusal was reported as the issuer
# being unreachable.
TENANT ?= 11111111-1111-4111-8111-11111111111a

.DEFAULT_GOAL := help
.PHONY: help env issuer run token api build fmt vet test test-unit test-integration \
        gates tidy arch stop migrate-status clean

help:
	@echo Targets:
	@echo   make env               copy .env.example to .env (does not overwrite)
	@echo   make issuer            terminal 1: the dev token issuer on $(ISSUER)
	@echo   make run               terminal 2: the service on $(ADDR)
	@echo   make token             save a provider token to $(TOKEN_FILE)
	@echo   make token ROLE=tenant a Tenant-scoped token: 403 on a provider route
	@echo   make token ROLE=both   authentic, confers no scope: also 403
	@echo   make api P=/v1/tenants/ID              GET a path with that token
	@echo   make api M=POST P=/v1/organizations B=body.json    send a body
	@echo   make stop              free $(ISSUER) and $(ADDR) after a stale run
	@echo   make gates             everything CI runs: fmt vet build arch tidy test
	@echo   make test-unit         no database needed
	@echo   make test-integration  requires .env and a running PostgreSQL
	@echo   make migrate-status    what Atlas thinks the database is at

# Not a copy that overwrites: .env holds working local credentials, and clobbering it
# from a template is the kind of loss that is noticed one debugging hour later.
env:
	@if exist .env (echo .env already exists -- leaving it alone) else (copy .env.example .env >nul && echo Created .env from .env.example)

# ---------------------------------------------------------------------------
# Running it by hand
# ---------------------------------------------------------------------------

# The build tag is the deployment boundary, not a flag: without -tags devissuer this
# command does not compile, so the token issuer cannot reach an image by accident.
issuer:
	go run -tags devissuer ./cmd/organization-devissuer

run:
	@if not exist .env (echo No .env yet. Run: make env && exit 1)
	go run ./cmd/organization-control

# The token is stored as a complete header line and handed to curl as -H @file, rather
# than read into a variable. Each recipe line is one `cmd /c`, so cmd expands %TOKEN%
# while parsing the line -- before the `set` on that same line has run. The header
# arrived as "Bearer " with nothing after it, and the service answered 401, which reads
# as the token being rejected rather than as never having been sent.
#
# --fail-with-body, not -f: the issuer explains its own refusals in the body, and -f
# discarded that. `make token ROLE=tenant` was answered "role=tenant needs a tenant_id"
# and reported here as "Could not reach the issuer" -- a guess printed over the answer.
# Only ROLE=tenant carries it. ROLE=both mints its own Tenant claim, because its purpose is
# to be refused: provider authority and authority over one Tenant in the same token.
TENANT_QUERY = $(if $(filter tenant,$(ROLE)),&tenant_id=$(TENANT),)

token:
	@curl.exe -sS --fail-with-body "http://$(ISSUER)/token?role=$(ROLE)$(TENANT_QUERY)" -o $(TOKEN_FILE).raw || (echo Issuer refused, or is not running on http://$(ISSUER) -- start it with: make issuer && type $(TOKEN_FILE).raw 2>nul && del $(TOKEN_FILE).raw 2>nul && exit 1)
	@for /f "delims=" %%t in ($(TOKEN_FILE).raw) do @echo Authorization: Bearer %%t> $(TOKEN_FILE)
	@del $(TOKEN_FILE).raw
	@echo Saved a $(ROLE) token to $(TOKEN_FILE)

M ?= GET
B ?=

# X-Administrative-Reason is not decoration: every provider-scoped call writes a row to
# audit.privileged_access, and the service refuses the call with 400 when the header is
# absent rather than recording an unexplained one.
REASON ?= driving the service by hand

api:
	@if "$(P)"=="" (echo Usage: make api P=/v1/tenants/ID  ^|^|  make api M=POST P=/v1/organizations B=body.json && exit 1)
	@if not exist $(TOKEN_FILE) (echo No token yet. Run: make token && exit 1)
	@curl.exe -s -i -X $(M) \
	  -H @$(TOKEN_FILE) \
	  -H "X-Administrative-Reason: $(REASON)" \
	  $(if $(B),-H "Content-Type: application/json" --data-binary "@$(B)",) \
	  $(if $(KEY),-H "Idempotency-Key: $(KEY)",) \
	  "http://$(ADDR)$(P)"

# ---------------------------------------------------------------------------
# Gates
# ---------------------------------------------------------------------------

build:
	go build ./...
	go build -tags devissuer ./...

# gofmt -l reports by printing names and exits 0 either way, so the check is whether it
# printed anything. findstr is the test rather than `for /f`, which exits 1 over an empty
# file -- the target failed on a clean tree while printing no filename, which is worse
# than not having the gate: it says the code is unformatted and does not say where.
fmt:
	@gofmt -l . > .fmt.tmp
	@findstr /r /c:"." .fmt.tmp >nul && (echo Not gofmt-clean: && type .fmt.tmp && del .fmt.tmp && exit 1) || (del .fmt.tmp && echo gofmt clean)

vet:
	go vet ./...

arch:
	go run github.com/anshacerbia2/foundation-platform/tools/archcheck ./...

tidy:
	go mod tidy
	@git diff --exit-code go.mod go.sum || (echo go.mod or go.sum changed -- commit the result && exit 1)

# -p 1 because the integration tests share one database and create and drop schema in
# it; run in parallel they interfere and fail in ways that look like locking bugs.
test:
	go test -race -p 1 ./...

test-unit:
	go test -race -short ./...

# REQUIRE_INTEGRATION turns a skip into a failure. Without it, an unreachable database
# makes the whole suite pass while testing nothing that touches PostgreSQL.
test-integration:
	@if not exist .env (echo No .env yet. Run: make env && exit 1)
	set REQUIRE_INTEGRATION=1&& go test -race -p 1 ./internal/...

gates: fmt vet build arch tidy test
	@echo All gates passed.

# Both ports are loopback and both processes are development-only, so this is safe in a way
# it would not be against anything shared. It exists because the alternative is netstat,
# reading a PID out of a column, and taskkill -- three steps to undo one stale `make run`.
#
# The loop is parenthesised and `& exit 0` sits outside it, because `for /f` over a command
# that printed nothing exits 1: with both ports already free the target failed while having
# done exactly what was asked. Inside the `do` body the `& exit 0` runs per iteration, so
# with zero iterations it never ran at all -- which is how it was written first.
#
# The second line is the real check, and it fails if anything still holds either port.
stop:
	@(for /f "tokens=5" %%p in ('netstat -ano ^| findstr /r /c:"$(ISSUER) .*LISTENING" /c:"$(ADDR) .*LISTENING"') do @(taskkill /f /pid %%p >nul 2>&1 && echo Stopped pid %%p)) & exit 0
	@netstat -ano | findstr /r /c:"$(ISSUER) .*LISTENING" /c:"$(ADDR) .*LISTENING" >nul && (echo Still listening -- something else holds $(ISSUER) or $(ADDR) && exit 1) || echo Both ports free.

migrate-status:
	atlas migrate status --env local

clean:
	go clean -cache -testcache
	@if exist $(TOKEN_FILE) del $(TOKEN_FILE)
