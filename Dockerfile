# The binary is cross-compiled with Go beforehand (see the CI workflow and the
# README's Development section) and copied in, so building the image never
# compiles anything — and never needs QEMU emulation for the other platform.
FROM gcr.io/distroless/static-debian12
ARG TARGETARCH
WORKDIR /
COPY dist/linux/${TARGETARCH}/albumreq /albumreq
# Beyond loopback on purpose: inside the container the port is reached through
# the container network. There is no login yet — do not publish it.
ENV ALBUMREQ_ADDR=:8768
ENTRYPOINT ["/albumreq"]
