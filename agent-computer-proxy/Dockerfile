# Static binary on scratch; built by `make proxy-image`.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/agent-computer-proxy .

FROM scratch
COPY --from=build /out/agent-computer-proxy /agent-computer-proxy
USER 65532:65532
ENTRYPOINT ["/agent-computer-proxy"]
