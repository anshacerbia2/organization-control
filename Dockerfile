# syntax=docker/dockerfile:1
#
# Two images from one build (STD-GLB-009 §Development Server Deployment):
#
#   service   the Organization Control service, on distroless static as a non-root user
#   migrate   the Control Database pipeline and the one-off tasks (bootstrap-provider, maintenance),
#             on the Postgres image, because the pipeline needs psql
#
# Every base is pinned by digest. The tag each digest was resolved from is beside it.

# golang:1.26.9-alpine
FROM golang@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS build
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN if [ "$GOPROXY" = "direct" ]; then apk add --no-cache git; fi && go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/organization-control ./cmd/organization-migrate

# arigaio/atlas:1.3.9, resolved from Docker Hub on 2026-11-10. 1.3.4 carried andybalholm/brotli v1.2.0
# (GO-2026-6888, Critical, in the CISA KEV catalogue) and klauspost/compress v1.18.7 (GO-2026-6865,
# High), which the gate found from 2026-11-05 (TDD-organization-control-001 §The Migrate Image and
# Its Exceptions).
FROM arigaio/atlas@sha256:ce36679f6e253ff13ec0c9cb07787c8c7b02934ad9b76c969977570391d696a8 AS atlas

# postgres:17.12-alpine3.24, resolved from Docker Hub on 2026-11-13 --
# the same image the Control Database runs, so psql matches the server.
FROM postgres@sha256:34261f594522cf41dfcb76aefd42b9d201098b0e6b23c9c68733f169e4218b6f AS migrate
COPY --from=atlas /atlas /usr/local/bin/atlas
# Atlas checks for a newer release over HTTPS around every command unless ATLAS_NO_UPDATE_NOTIFIER is
# set, and may send anonymous telemetry unless ATLAS_NO_ANON_TELEMETRY is true. With both off it
# connects to nothing but the database. scripts/atlas-execute-path.sh proves it on every scan, so a
# new advisory in this file is decided against current evidence (TDD-organization-control-001 §The
# Migrate Image and Its Exceptions).
ENV ATLAS_NO_UPDATE_NOTIFIER=true \
    ATLAS_NO_ANON_TELEMETRY=true
# organization-control is here for its bootstrap-provider subcommand, a one-off task.
COPY --from=build /out/organization-migrate /out/organization-control /usr/local/bin/
WORKDIR /work
COPY atlas.hcl schema.hcl ./
COPY migrations ./migrations
COPY scripts/login-roles.sql ./login-roles.sql
COPY deploy/dev/migrate.sh /usr/local/bin/organization-dev-migrate
RUN chmod 0755 /usr/local/bin/organization-dev-migrate
USER postgres
ENTRYPOINT ["organization-dev-migrate"]

# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS service
COPY --from=build /out/organization-control /organization-control
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/organization-control"]
