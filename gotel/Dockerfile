# --------- Stage 1: build Gotell with the full go toolchain ----------------
FROM golang:1.27 AS build
WORKDIR /src

# Download dependencies 
COPY go.mod go.sum ./
RUN go mod download


# Build one static linux binary , no C libraries needed (pure Go - SQLite).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gotell .
# the data folder, owned by the unprivilaged user that will run Gotell.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# --- Stage 2: the image people run-------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gotell /gotell
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
CMD [ "/gotell", "-healthcheck", "-listen", "0.0.0.0:8080"]

ENTRYPOINT [ "/gotell" ]
CMD ["-listen", "0.0.0.0:8080", "-db", "/data/gotell.db"]