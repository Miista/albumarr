# The binary is cross-compiled with Go beforehand (see the CI workflow and the README's
# Development section) and copied in, so building the image never compiles anything, and
# never needs QEMU emulation for the other platform. distroless/static has no shell and no
# package manager: nothing in the image but the binary, CA certificates and tzdata.
# The data directory is made in a throwaway stage, because the final image has no shell. It
# runs on the build machine's own platform (it only makes a directory), so it needs no emulation.
FROM --platform=$BUILDPLATFORM busybox:1.38 AS data
RUN mkdir -p /out/app/data && chown 65532:65532 /out/app/data && chmod 1777 /out/app/data

FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETARCH
COPY dist/linux/${TARGETARCH}/albumarr /albumarr

# The container runs as the image's unprivileged user (65532), or as any user you choose with
# --user / `user:` (1000:1000, say). /app/data is world-writable with the sticky bit, like
# /tmp, so a fresh named volume (which copies this directory's permissions) is writable by
# whoever runs the container; what albumarr writes inside it is private to that user (files
# 0600, directories 0700). A bind-mounted directory must be writable by the user it runs as.
COPY --from=data /out/ /
WORKDIR /app/data
VOLUME /app/data

# Beyond loopback on purpose: inside the container the port is reached through the
# container network. A fresh install has no login yet: the FIRST visitor creates the admin
# account (or set the ALBUMARR_OIDC_* variables to use your identity provider, which keeps
# that closed). Put it behind your proxy or on a private network before anyone can reach it.
ENV ALBUMARR_ADDR=:8768 \
    ALBUMARR_DATA_DIR=/app/data
EXPOSE 8768

# There is no shell or curl in the image: the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/albumarr", "healthcheck"]

ENTRYPOINT ["/albumarr"]
