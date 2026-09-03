# A workload for NEXUS to deploy. Kept minimal on purpose.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY main.go go.mod ./
ARG VERSION=1.0.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/service .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/service"]
