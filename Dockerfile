# Image of the aluna binary: docker build --build-arg VERSION=0.1.0 -t aluna:0.1.0 .
# Both bases are pinned by digest; a new digest is a reviewed change, not a rebuild that drifts.

FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
# Only what the binary embeds and compiles: no .git, no local .env files in the build stage.
COPY api ./api
COPY cmd ./cmd
COPY config ./config
COPY internal ./internal
COPY migrations ./migrations
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" -o /out/aluna ./cmd/aluna

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/aluna /aluna
USER nonroot:nonroot
EXPOSE 8080 8081
# Probes the api's /healthz; a worker container has no HTTP listener and runs with --no-healthcheck.
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=3 CMD ["/aluna", "healthcheck"]
ENTRYPOINT ["/aluna"]
CMD ["api"]
