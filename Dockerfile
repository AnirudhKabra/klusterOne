FROM golang:1.21 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ko-controller ./cmd/manager

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/ko-controller /ko-controller
USER 65532:65532
ENTRYPOINT ["/ko-controller"]
