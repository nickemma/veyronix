FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/plinthd ./cmd/plinthd && \
    CGO_ENABLED=0 go build -o /out/plinth-operator ./operator

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/plinthd /plinthd
COPY --from=build /out/plinth-operator /plinth-operator
COPY --from=build /src/api /app/api
USER nonroot:nonroot
