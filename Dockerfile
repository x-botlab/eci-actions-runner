# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X github.com/x-botlab/eci-actions-runner/internal/cli.Version=${VERSION}" \
      -o /out/eci-runner ./cmd/eci-runner

# distroless static carries the CA bundle, which the GitHub API client needs.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/eci-runner /eci-runner
USER nonroot:nonroot
EXPOSE 9090
ENTRYPOINT ["/eci-runner"]
CMD ["server", "--config", "/etc/eci-runner/pools.yaml"]
