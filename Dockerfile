# verify-proxy as a container: a static binary on distroless, non-root.
#
#   docker build -t verify-proxy .
#   docker run -p 8080:8080 -e UPSTREAM_URL=http://my-mcp:3000/mcp \
#     -e NEXUS_REGISTERED_HOST=... -e NEXUS_CREATOR_KEY=... -e NEXUS_PUBLIC_KEY=... \
#     verify-proxy
#
# For a stdio server, build your own image FROM your server's and copy the
# binary in (see README.md): a stdio server must run beside the proxy.
FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /verify-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /verify-proxy /verify-proxy
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/verify-proxy"]
