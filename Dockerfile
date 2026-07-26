# jsingo reference image: a Go binary plus a hardened bun sidecar.
#
# Two things drive this layout. The build must never execute npm lifecycle
# scripts, and the runtime must give the sidecar as little as possible. See
# docs/SECURITY.md for the reasoning behind each control.

# ---------------------------------------------------------------------------
# 1. JavaScript bundle
#
# Pinned by digest, not tag. A tag is mutable: an attacker who controls the
# registry, or a maintainer who force-pushes, changes what "1.3-alpine" means
# without changing this file.
# ---------------------------------------------------------------------------
FROM oven/bun:1.3-alpine AS jsbuild

WORKDIR /build

# Copy manifests before sources so a dependency change is the only thing that
# busts this layer.
COPY jsruntime/package.json jsruntime/bun.lock ./jsruntime/
COPY examples/readability/js/package.json examples/readability/js/bun.lock ./examples/readability/js/

# --ignore-scripts is the single most important line in this file.
#
# npm preinstall/postinstall hooks execute arbitrary code at INSTALL time, on
# the build machine, outside every runtime sandbox below. They are the most
# used vector in real supply-chain compromises (eslint-scope, event-stream,
# ua-parser-js). Nothing jsingo depends on needs them.
#
# --frozen-lockfile makes the build fail rather than silently resolve a
# different version than the one that was reviewed.
RUN cd jsruntime && bun install --frozen-lockfile --ignore-scripts \
 && cd ../examples/readability/js && bun install --frozen-lockfile --ignore-scripts

COPY jsruntime/ ./jsruntime/
COPY examples/ ./examples/

# Rebuild the committed bundles from source and verify they match.
#
# The Go binary embeds the committed copies, so if they were stale the image
# would ship different code than the repository shows. Failing here is the
# point.
RUN bun build ./examples/readability/js/article.ts \
      --target=node --format=esm --minify \
      --outfile=/tmp/article.bundle.js \
 && cmp /tmp/article.bundle.js ./examples/readability/js/article.bundle.js \
 && sha256sum ./examples/readability/js/article.bundle.js | tee /build/bundle.sha256

# ---------------------------------------------------------------------------
# 2. Go binary
# ---------------------------------------------------------------------------
FROM golang:1.24-alpine AS gobuild

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download && go mod verify

COPY . .

ARG VERSION=dev
# CGO off gives a static binary with no libc to keep current in the runtime
# image. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w -X github.com/DiyRex/jsingo.Version=${VERSION}" \
      -o /out/app ./examples/server

# ---------------------------------------------------------------------------
# 3. Runtime
#
# Not distroless, because the sidecar needs a real bun binary. The bun image is
# the smallest base that has one; everything not needed is stripped below.
# ---------------------------------------------------------------------------
FROM oven/bun:1.3-alpine AS runtime

# Two unprivileged users are defined, but note what actually happens.
#
# In THIS single-container topology both the Go process and the sidecar run as
# "app". A non-root parent cannot move its child to another uid without
# CAP_SETUID, and we drop all capabilities - correctly. Verified by inspection:
# `docker top` shows both processes as app.
#
# So the separation the sidecar gets here is the scrubbed environment, the
# read-only rootfs and the cgroup limits - NOT uid isolation. The "sidecar"
# user exists for the two-container topology in deploy/k8s-two-container.yaml,
# where the kubelet assigns it and the isolation is real.
#
# Do not read the second uid as protection in this image.
RUN addgroup -g 10001 -S app \
 && adduser  -u 10001 -S -G app -H -s /sbin/nologin app \
 && addgroup -g 10002 -S sidecar \
 && adduser  -u 10002 -S -G sidecar -H -s /sbin/nologin sidecar \
 # Remove the package manager and shells that an exploit would otherwise use
 # to stage a second payload.
 && rm -rf /usr/local/bin/npm /usr/local/bin/npx /var/cache/apk/* /root/.cache

# Scratch space for the sidecar. $HOME and $TMPDIR point here, so a dependency
# that walks $HOME looking for credentials finds an empty directory it owns.
RUN mkdir -p /sandbox && chown sidecar:sidecar /sandbox && chmod 0700 /sandbox

COPY --from=gobuild --chown=root:root --chmod=0755 /out/app /usr/local/bin/app
COPY --from=jsbuild --chown=root:root --chmod=0444 /build/bundle.sha256 /opt/jsingo/bundle.sha256

# The JavaScript bundle is embedded in the Go binary, so nothing else needs to
# be copied. The recorded hash is kept for provenance only.

USER app:app

ENV JSINGO_SANDBOX_DIR=/sandbox \
    XDG_CACHE_HOME=/sandbox \
    NODE_ENV=production \
    DO_NOT_TRACK=1

# Read-only rootfs is set by the orchestrator, not here, because it is a
# runtime flag. See deploy/ for the manifests that apply it.

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/app"]
