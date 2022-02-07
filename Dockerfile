FROM amazonlinux:2 AS builder

FROM scratch
COPY proxy-ecs /proxy

# Get CA certificates
COPY --from=builder /etc/pki /etc/pki
ENTRYPOINT ["/proxy"]