# A sidecar that gives chaos-operator the tools its faults need. The sample
# service is distroless, so it has no tc and no stress-ng. Pods share one
# network namespace, so tc in this container slows the whole pod.
#
# It runs as root on purpose. tc needs CAP_NET_ADMIN, and Linux does not give
# an added capability to a non-root process. The pod spec grants NET_ADMIN to
# this container only.
FROM alpine:3.22
RUN apk add --no-cache iproute2-tc stress-ng
ENTRYPOINT ["sleep", "infinity"]
