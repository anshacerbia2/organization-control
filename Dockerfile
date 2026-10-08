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

# arigaio/atlas:1.3.3
FROM arigaio/atlas@sha256:07f3f92fa46e684ed789d5ef344a25494a4fa6844ef1ea1fa4e138522c2c37ac AS atlas

# postgres:17.11-alpine -- the same image the Control Database runs, so psql matches the server.
FROM postgres@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24 AS migrate
COPY --from=atlas /atlas /usr/local/bin/atlas
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
