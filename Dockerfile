# =============================================================================
# Stage 1 — Build
# =============================================================================
# We use a full Go image (not alpine) for the build stage to ensure all
# standard C libraries are available for cgo-free builds. The resulting binary
# is statically linked so the final image needs no libc at all.
FROM golang:1.26 AS builder

WORKDIR /src

# Copy the module manifests first and download dependencies in a separate
# layer. Docker caches this layer as long as go.mod and go.sum don't change,
# so routine code edits don't trigger a full re-download.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code.
COPY . .

# Build a statically-linked binary.
#   CGO_ENABLED=0  — no C bindings; the binary has zero dynamic dependencies.
#   -trimpath      — remove local file-system paths from stack traces (security).
#   -ldflags "-s -w" — strip debug symbols and DWARF info to shrink the binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /kube-sentinel \
      .

# =============================================================================
# Stage 2 — Final image
# =============================================================================
# distroless/static contains only:
#   - CA certificates (for TLS to the Kubernetes API)
#   - /etc/passwd and /etc/group (so the nonroot user exists)
#   - timezone data
# There is no shell, no package manager, and no interpreter — the attack
# surface is as small as possible.
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the compiled binary from the builder stage.
COPY --from=builder /kube-sentinel /kube-sentinel

# Copy the default config. Users can override it at runtime by mounting
# a ConfigMap at /config.yaml (see README for the Deployment example).
COPY --from=builder /src/config.yaml /config.yaml

# The :nonroot image tag already sets the default user to UID 65532.
# We declare it explicitly so `docker inspect` and scanners report it clearly.
USER nonroot:nonroot

# Expose the metrics / healthz port.
EXPOSE 8080

ENTRYPOINT ["/kube-sentinel"]
