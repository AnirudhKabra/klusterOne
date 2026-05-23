FROM golang:1.21 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/fury-controller ./cmd/manager

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/fury-controller /fury-controller
USER 65532:65532
ENTRYPOINT ["/fury-controller"]
