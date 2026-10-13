# The binary is cross-compiled with Go beforehand (see the CI workflow and the README's
# Development section) and copied in, so building the image never compiles anything, and
# never needs QEMU emulation for the other platform. distroless/static has no shell and no
# package manager: nothing in the image but the binary, CA certificates and tzdata.
FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETARCH
COPY dist/linux/${TARGETARCH}/albumreq /albumreq

# The container runs as the image's unprivileged user (65532). /app/data is created owned by
# that user, so a fresh named volume is writable; a bind-mounted directory must be owned by
# 65532 too (chown 65532:65532 <dir>), or run the container with --user matching its owner.
WORKDIR /app/data
VOLUME /app/data

# Beyond loopback on purpose: inside the container the port is reached through the
# container network. A fresh install has no login yet: the FIRST visitor creates the admin
# account (or set the ALBUMREQ_OIDC_* variables to use your identity provider, which keeps
# that closed). Put it behind your proxy or on a private network before anyone can reach it.
ENV ALBUMREQ_ADDR=:8768 \
    ALBUMREQ_DATA_DIR=/app/data
EXPOSE 8768

# There is no shell or curl in the image: the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/albumreq", "healthcheck"]

ENTRYPOINT ["/albumreq"]
