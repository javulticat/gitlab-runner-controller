FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gitlab-runner-controller ./cmd/gitlab-runner-controller \
    && mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch AS runtime
ARG VERSION=dev
LABEL org.opencontainers.image.title="GitLab Runner Controller" \
      org.opencontainers.image.description="Provider-neutral autoscaling for GitLab runner hosts" \
      org.opencontainers.image.source="https://github.com/javulticat/gitlab-runner-controller" \
      org.opencontainers.image.url="https://github.com/javulticat/gitlab-runner-controller" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gitlab-runner-controller /usr/local/bin/gitlab-runner-controller
COPY --from=build --chown=65532:65532 /out/data /data
COPY controller.example.json /etc/gitlab-runner-controller/controller.json
COPY LICENSE /LICENSE
USER 65532:65532
ENV PORT=3000 STORAGE_DIR=/data CONTROLLER_CONFIG=/etc/gitlab-runner-controller/controller.json
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/gitlab-runner-controller", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/gitlab-runner-controller"]
CMD ["serve"]

FROM build AS fixture-build
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o /out/provider-fixture ./internal/testprovider

FROM runtime AS smoke
COPY --from=fixture-build /out/provider-fixture /opt/provider-fixture

FROM runtime AS final
