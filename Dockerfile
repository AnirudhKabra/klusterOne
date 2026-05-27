FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY bin/ko-controller /ko-controller
USER 65532:65532
ENTRYPOINT ["/ko-controller"]
